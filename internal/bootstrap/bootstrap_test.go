package bootstrap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/feliphexavier/jungleGamingProjeto/internal/bootstrap"
	"github.com/feliphexavier/jungleGamingProjeto/internal/config"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth/authtest"
	"github.com/feliphexavier/jungleGamingProjeto/internal/testsupport/pgtest"
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

func testConfig(t *testing.T, dbURL string) (config.Config, string) {
	t.Helper()
	port := freePort(t)
	return config.Config{
		HTTPAddr:         "127.0.0.1:" + port,
		DatabaseURL:      dbURL,
		OIDC:             authtest.NewIssuer(t).Config(),
		ShutdownTimeout:  10 * time.Second,
		LogLevel:         slog.LevelError,
		PendingInterval:  50 * time.Millisecond,
		PendingBatchSize: 10,
	}, "http://127.0.0.1:" + port
}

func TestGraphIsValid(t *testing.T) {
	cfg, _ := testConfig(t, "postgres://u:p@localhost:1/db")
	if err := fx.ValidateApp(bootstrap.Options(cfg)); err != nil {
		t.Fatalf("grafo de dependências inválido: %v", err)
	}
}

// Sobe a aplicação completa, verifica que atende, e encerra verificando que o
// worker terminou, o HTTP parou e o pool foi fechado.
func TestStartAndStop(t *testing.T) {
	db := pgtest.New(t)
	cfg, baseURL := testConfig(t, db.URL)

	var (
		pool   *pgxpool.Pool
		worker bootstrap.PendingReferenceWorker
	)
	application := fxtest.New(t, bootstrap.Options(cfg), fx.Populate(&pool, &worker))
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

	stopped := worker.Stopped()
	application.RequireStop()

	select {
	case <-stopped:
	default:
		t.Error("worker ainda rodando após o encerramento")
	}
	if _, err := http.Get(baseURL + "/health/live"); err == nil {
		t.Error("HTTP ainda aceita conexões após o encerramento")
	}
	if err := pool.Ping(context.Background()); err == nil {
		t.Error("pool do postgres ainda aberto após o encerramento")
	}
}

func TestStartFailsWithoutDatabase(t *testing.T) {
	cfg, _ := testConfig(t, "postgres://u:p@127.0.0.1:1/db?connect_timeout=1")
	application := fx.New(bootstrap.Options(cfg), fx.NopLogger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := application.Start(ctx); err == nil {
		_ = application.Stop(ctx)
		t.Fatal("a aplicação subiu sem banco")
	}
}

func TestStartFailsWhenPortBusy(t *testing.T) {
	db := pgtest.New(t)
	cfg, _ := testConfig(t, db.URL)
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	application := fx.New(bootstrap.Options(cfg), fx.NopLogger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := application.Start(ctx); err == nil {
		_ = application.Stop(ctx)
		t.Fatal("a aplicação subiu com a porta ocupada")
	}
}
