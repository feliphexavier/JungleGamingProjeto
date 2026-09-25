package bootstrap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/bootstrap"
	"github.com/feliphexavier/jungleGamingProjeto/internal/config"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth/authtest"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/postgres"
	sqsinfra "github.com/feliphexavier/jungleGamingProjeto/internal/infra/sqs"
	"github.com/feliphexavier/jungleGamingProjeto/internal/testsupport/pgtest"
	"github.com/feliphexavier/jungleGamingProjeto/internal/testsupport/sqstest"
)

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
}

func testConfig(t *testing.T, dbURL string, sqsCfg sqsinfra.Config) (config.Config, string, *authtest.Issuer) {
	t.Helper()
	port := freePort(t)
	idp := authtest.NewIssuer(t)
	return config.Config{
		HTTPAddr:            "127.0.0.1:" + port,
		DatabaseURL:         dbURL,
		OIDC:                idp.Config(),
		ShutdownTimeout:     10 * time.Second,
		LogLevel:            slog.LevelError,
		PendingInterval:     50 * time.Millisecond,
		PendingBatchSize:    10,
		SQS:                 sqsCfg,
		ConsumerWait:        time.Second,
		ConsumerMaxMessages: 10,
		ConsumerTimeout:     10 * time.Second,
		OutboxInterval:      50 * time.Millisecond,
		OutboxBatchSize:     10,
		OutboxLease:         10 * time.Second,
		OutboxMaxBackoff:    time.Second,
	}, "http://127.0.0.1:" + port, idp
}

// unreachableSQS: configuração válida apontando para um endpoint sem SQS.
var unreachableSQS = sqsinfra.Config{
	Endpoint: "http://127.0.0.1:1", Region: "us-east-1",
	InboundQueue: "in.fifo", DLQ: "dlq.fifo", EventsQueue: "events.fifo",
}

func TestGraphIsValid(t *testing.T) {
	cfg, _, _ := testConfig(t, "postgres://u:p@localhost:1/db", unreachableSQS)
	if err := fx.ValidateApp(bootstrap.Options(cfg)); err != nil {
		t.Fatalf("grafo de dependências inválido: %v", err)
	}
}

// Sobe a aplicação completa (PostgreSQL e SQS reais), processa uma operação
// recebida pelo SQS até o evento publicado, e encerra verificando que os
// workers terminaram, o HTTP parou e o pool foi fechado.
func TestStartAndStop(t *testing.T) {
	db := pgtest.New(t)
	q := sqstest.New(t)
	cfg, baseURL, idp := testConfig(t, db.URL, q.Config)

	var (
		pool     *pgxpool.Pool
		outbox   bootstrap.OutboxWorker
		pending  bootstrap.PendingReferenceWorker
		consumer bootstrap.ConsumerWorker
	)
	application := fxtest.New(t, bootstrap.Options(cfg), fx.Populate(&pool, &outbox, &pending, &consumer))
	application.RequireStart()

	res, err := http.Get(baseURL + "/health/ready")
	if err != nil {
		t.Fatalf("readiness após subir: %v", err)
	}
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || body["status"] != "UP" {
		t.Fatalf("ready = %d %v", res.StatusCode, body)
	}

	// Rota de negócio exige autenticação também na aplicação composta.
	res, err = http.Get(baseURL + "/wagering/transactions/00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("rota de negócio sem token = %d, want 401", res.StatusCode)
	}

	// Operação pelo SQS: consumida, processada e com eventos publicados.
	svc := app.NewService(postgres.NewStore(pool))
	w, err := svc.OpenWallet(context.Background(), app.OpenWalletCommand{
		Principal: app.Principal{Internal: true}, PlayerID: uuid.NewString(), Amount: "100.00", Currency: "BRL",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := fmt.Sprintf(`{"messageId":"m-1","type":"WagerOperationRequested","occurredAt":"2026-09-25T12:00:00Z",
		"data":{"idempotencyKey":"provider-a:bet-1","providerId":"provider-a","externalTransactionId":"bet-1",
		"playerId":%q,"walletId":%q,"roundId":"r","gameId":"g","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}`,
		w.PlayerID(), w.ID())
	q.Send(t, q.Queues.Inbound, msg, w.ID().String(), "m-1", idp.ProviderToken(t, "provider-a"))

	var events []string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && len(events) < 4 {
		for _, m := range q.Drain(t, q.Queues.Events) {
			var env struct {
				EventType string `json:"eventType"`
			}
			_ = json.Unmarshal([]byte(*m.Body), &env)
			events = append(events, env.EventType)
		}
	}
	// Abertura (Processed + BalanceChanged) e aposta (Processed + BalanceChanged).
	if len(events) != 4 || strings.Count(strings.Join(events, ","), "WalletBalanceChanged") != 2 {
		t.Errorf("eventos publicados = %v", events)
	}
	if got, _ := svc.GetWallet(context.Background(), app.Principal{Internal: true}, w.ID()); got == nil || got.Balance().String() != "75.00 BRL" {
		t.Errorf("saldo após mensagem SQS = %v", got)
	}

	loops := map[string]<-chan struct{}{
		"outbox": outbox.Stopped(), "pending": pending.Stopped(), "consumer": consumer.Stopped(),
	}
	application.RequireStop()

	for name, stopped := range loops {
		select {
		case <-stopped:
		default:
			t.Errorf("worker %s ainda rodando após o encerramento", name)
		}
	}
	if _, err := http.Get(baseURL + "/health/live"); err == nil {
		t.Error("HTTP ainda aceita conexões após o encerramento")
	}
	if err := pool.Ping(context.Background()); err == nil {
		t.Error("pool do postgres ainda aberto após o encerramento")
	}
}

func startMustFail(t *testing.T, cfg config.Config, what string) {
	t.Helper()
	application := fx.New(bootstrap.Options(cfg), fx.NopLogger)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := application.Start(ctx); err == nil {
		_ = application.Stop(ctx)
		t.Fatalf("a aplicação subiu %s", what)
	}
}

func TestStartFailsWithoutDatabase(t *testing.T) {
	cfg, _, _ := testConfig(t, "postgres://u:p@127.0.0.1:1/db?connect_timeout=1", unreachableSQS)
	startMustFail(t, cfg, "sem banco")
}

func TestStartFailsWithoutQueue(t *testing.T) {
	db := pgtest.New(t)
	q := sqstest.New(t)
	sqsCfg := q.Config
	sqsCfg.EventsQueue = "fila-inexistente.fifo"
	cfg, _, _ := testConfig(t, db.URL, sqsCfg)
	startMustFail(t, cfg, "sem a fila de eventos")
}

func TestStartFailsWhenPortBusy(t *testing.T) {
	db := pgtest.New(t)
	q := sqstest.New(t)
	cfg, _, _ := testConfig(t, db.URL, q.Config)
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	startMustFail(t, cfg, "com a porta ocupada")
}
