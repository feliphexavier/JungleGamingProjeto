// Package pgtest cria bancos PostgreSQL descartáveis para testes de integração.
//
// Os testes usam um PostgreSQL real (o do Docker Compose). Cada chamada a New
// cria um banco novo, aplica as migrations e o remove ao final do teste: como o
// ledger é append-only, não há como limpar tabelas entre testes.
//
// A conexão administrativa vem de TEST_DATABASE_URL; sem ela, o teste é pulado.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/feliphexavier/jungleGamingProjeto/migrations"
)

// EnvURL é a variável com a URL de um PostgreSQL onde o teste pode criar bancos.
const EnvURL = "TEST_DATABASE_URL"

// DB é um banco de teste com as migrations aplicadas.
type DB struct {
	Pool *pgxpool.Pool
	URL  string
}

// New cria o banco de teste. Pula o teste se TEST_DATABASE_URL não estiver definida.
func New(t testing.TB) *DB {
	t.Helper()
	adminURL := os.Getenv(EnvURL)
	if adminURL == "" {
		t.Skipf("%s não definida: teste de integração pulado", EnvURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	name := "test_" + randomSuffix(t)
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("pgtest: conectar em %s: %v", EnvURL, err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("pgtest: criar banco: %v", err)
	}

	dbURL := withDatabase(t, adminURL, name)
	t.Cleanup(func() { dropDatabase(adminURL, name) })

	migrate(t, ctx, dbURL)

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("pgtest: pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return &DB{Pool: pool, URL: dbURL}
}

func migrate(t testing.TB, ctx context.Context, dbURL string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("pgtest: abrir banco: %v", err)
	}
	defer sqlDB.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("pgtest: goose: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("pgtest: migrations: %v", err)
	}
}

func dropDatabase(adminURL, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return
	}
	defer admin.Close(ctx)
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

func withDatabase(t testing.TB, rawURL, name string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("pgtest: URL inválida: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func randomSuffix(t testing.TB) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
