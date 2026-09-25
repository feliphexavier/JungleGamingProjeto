// Package bootstrap compõe a aplicação com Uber Fx: configuração, conexões,
// repositórios, casos de uso, handlers e workers, cada um em seu fx.Module.
//
// Ordem de ciclo de vida: o Fx executa os OnStart na ordem em que as
// dependências são construídas e os OnStop na ordem inversa. Como o servidor
// HTTP e os workers dependem do pool, o pool inicia primeiro e fecha por
// último, depois que ninguém mais o usa.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/config"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/httpapi"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/postgres"
	sqsinfra "github.com/feliphexavier/jungleGamingProjeto/internal/infra/sqs"
	"github.com/feliphexavier/jungleGamingProjeto/internal/worker"
)

// Options monta a aplicação completa. cfg é injetada (em produção vem de
// config.Load; nos testes, de valores controlados).
func Options(cfg config.Config) fx.Option {
	return fx.Options(
		fx.Supply(cfg),
		fx.StopTimeout(cfg.ShutdownTimeout),
		LoggerModule,
		PostgresModule,
		AppModule,
		AuthModule,
		SQSModule,
		// Workers sobem antes do HTTP; no encerramento (ordem inversa) o HTTP para
		// primeiro de aceitar entradas, depois o consumidor SQS, o worker de
		// pendências e o publicador da outbox (que ainda publica o que os outros
		// gravaram), e o pool fecha por último.
		WorkerModule,
		HTTPModule,
	)
}

var LoggerModule = fx.Module("logger",
	fx.Provide(func(cfg config.Config) *slog.Logger {
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	}),
	fx.WithLogger(func(log *slog.Logger) fxevent.Logger {
		l := &fxevent.SlogLogger{Logger: log.With(slog.String("component", "fx"))}
		l.UseLogLevel(slog.LevelDebug)
		return l
	}),
)

type healthOut struct {
	fx.Out
	Check httpapi.HealthCheck `group:"health"`
}

var PostgresModule = fx.Module("postgres",
	fx.Provide(newPool, postgres.NewStore,
		func(s *postgres.Store) app.Store { return s },
		func(s *postgres.Store) healthOut {
			return healthOut{Check: httpapi.HealthCheck{Name: "postgres", Check: s.Ping}}
		},
	),
)

// newPool cria o pool sem conectar; OnStart confirma a conexão (a aplicação
// não sobe sem banco) e OnStop fecha o pool.
func newPool(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := pool.Ping(ctx); err != nil {
				return fmt.Errorf("postgres indisponível na subida: %w", err)
			}
			log.Info("postgres conectado")
			return nil
		},
		OnStop: func(context.Context) error {
			pool.Close()
			log.Info("pool do postgres fechado")
			return nil
		},
	})
	return pool, nil
}

var AppModule = fx.Module("app",
	fx.Provide(func(store app.Store) *app.Service { return app.NewService(store) }),
)

var AuthModule = fx.Module("auth",
	fx.Provide(newVerifier,
		func(v *auth.Verifier) httpapi.Authenticator { return v },
	),
)

// newVerifier liga a busca de chaves do JWKS ao ciclo de vida da aplicação.
func newVerifier(lc fx.Lifecycle, cfg config.Config) (*auth.Verifier, error) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.StopHook(cancel))
	v, err := auth.NewVerifier(ctx, cfg.OIDC)
	if err != nil {
		cancel()
		return nil, err
	}
	return v, nil
}

type httpIn struct {
	fx.In
	Service *app.Service
	Auth    httpapi.Authenticator
	Checks  []httpapi.HealthCheck `group:"health"`
	Log     *slog.Logger
}

var HTTPModule = fx.Module("http",
	fx.Provide(
		func(in httpIn) *httpapi.Server {
			return httpapi.NewServer(in.Service, in.Auth, in.Checks, in.Log)
		},
		newHTTPServer,
	),
	fx.Invoke(func(*http.Server) {}),
)

// newHTTPServer abre a porta em OnStart (falha de bind impede a subida) e, em
// OnStop, para de aceitar conexões e espera as requisições em andamento.
func newHTTPServer(lc fx.Lifecycle, cfg config.Config, api *httpapi.Server, log *slog.Logger) *http.Server {
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			ln, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				return fmt.Errorf("http: %w", err)
			}
			log.Info("http escutando", slog.String("addr", ln.Addr().String()))
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Error("http encerrado com erro", slog.String("error", err.Error()))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			log.Info("http: parando de aceitar conexões")
			return srv.Shutdown(ctx)
		},
	})
	return srv
}

// PendingReferenceWorker retoma operações em PENDING_REFERENCE.
var SQSModule = fx.Module("sqs",
	fx.Provide(newSQSClient,
		func(c *awssqs.Client, q *sqsinfra.Queues) healthOut {
			return healthOut{Check: httpapi.HealthCheck{Name: "sqs", Check: func(ctx context.Context) error {
				_, err := c.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{QueueUrl: &q.Events})
				return err
			}}}
		},
	),
)

// newSQSClient cria o cliente e resolve as filas em OnStart: fila ausente ou
// SQS indisponível impede a subida.
func newSQSClient(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) (*awssqs.Client, *sqsinfra.Queues, error) {
	client, err := sqsinfra.NewClient(context.Background(), cfg.SQS)
	if err != nil {
		return nil, nil, err
	}
	queues := &sqsinfra.Queues{}
	lc.Append(fx.StartHook(func(ctx context.Context) error {
		if err := queues.Resolve(ctx, client, cfg.SQS); err != nil {
			return err
		}
		log.Info("filas sqs resolvidas")
		return nil
	}))
	return client, queues, nil
}

// Workers: tipos distintos para que o Fx os construa e os testes os observem.
type (
	OutboxWorker           struct{ *worker.Loop }
	PendingReferenceWorker struct{ *worker.Loop }
	ConsumerWorker         struct{ *worker.Loop }
)

// instanceID identifica esta instância nos leases da outbox.
func instanceID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), app.NewID().String()[:8])
}

var WorkerModule = fx.Module("workers",
	// Ordem de registro = ordem de início; o encerramento é o inverso.
	fx.Provide(
		func(lc fx.Lifecycle, cfg config.Config, store app.Store, c *awssqs.Client, q *sqsinfra.Queues, log *slog.Logger) OutboxWorker {
			relay := app.NewOutboxRelay(store, sqsinfra.NewPublisher(c, q), app.OutboxRelayConfig{
				Owner:      instanceID(),
				BatchSize:  cfg.OutboxBatchSize,
				Lease:      cfg.OutboxLease,
				MaxBackoff: cfg.OutboxMaxBackoff,
			})
			loop := worker.NewLoop("outbox-relay", cfg.OutboxInterval, func(ctx context.Context) error {
				n, err := relay.PublishBatch(ctx)
				if n > 0 {
					log.Debug("eventos publicados", slog.Int("count", n))
				}
				return err
			}, log)
			lc.Append(fx.Hook{OnStart: loop.Start, OnStop: loop.Stop})
			return OutboxWorker{loop}
		},
		func(lc fx.Lifecycle, cfg config.Config, svc *app.Service, log *slog.Logger) PendingReferenceWorker {
			loop := worker.NewLoop("pending-references", cfg.PendingInterval, func(ctx context.Context) error {
				n, err := svc.ResumePendingReferences(ctx, cfg.PendingBatchSize)
				if n > 0 {
					log.Info("referências pendentes retomadas", slog.Int("count", n))
				}
				return err
			}, log)
			lc.Append(fx.Hook{OnStart: loop.Start, OnStop: loop.Stop})
			return PendingReferenceWorker{loop}
		},
		func(lc fx.Lifecycle, cfg config.Config, svc *app.Service, auth httpapi.Authenticator, c *awssqs.Client, q *sqsinfra.Queues, log *slog.Logger) ConsumerWorker {
			consumer := sqsinfra.NewConsumer(c, auth, svc, sqsinfra.ConsumerConfig{
				Queues:         q,
				WaitTime:       cfg.ConsumerWait,
				MaxMessages:    int32(cfg.ConsumerMaxMessages),
				ProcessTimeout: cfg.ConsumerTimeout,
			}, log)
			// O long polling já espera por mensagens; o intervalo só separa ciclos.
			loop := worker.NewLoop("sqs-consumer", 50*time.Millisecond, func(ctx context.Context) error {
				_, err := consumer.Poll(ctx)
				return err
			}, log)
			lc.Append(fx.Hook{OnStart: loop.Start, OnStop: loop.Stop})
			return ConsumerWorker{loop}
		},
	),
	fx.Invoke(func(OutboxWorker, PendingReferenceWorker, ConsumerWorker) {}),
)
