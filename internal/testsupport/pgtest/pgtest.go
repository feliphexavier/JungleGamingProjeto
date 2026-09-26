// Package pgtest cria bancos PostgreSQL descartáveis para testes de integração.
//
// Os testes usam um PostgreSQL real (o do Docker Compose). Cada chamada a New
// cria um banco novo e o remove ao final do teste: como o ledger é append-only,
// não há como limpar tabelas entre testes.
//
// As migrations são aplicadas uma única vez num banco-modelo; cada banco de
// teste é uma cópia dele (CREATE DATABASE ... TEMPLATE), bem mais barata que
// migrar do zero. O nome do modelo inclui um hash das migrations, então uma
// migration nova ou alterada gera outro modelo.
//
// A conexão administrativa vem de TEST_DATABASE_URL; sem ela, o teste é pulado.
package pgtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

	tmpl, err := ensureTemplate(ctx, admin, adminURL)
	if err != nil {
		t.Fatalf("pgtest: banco-modelo: %v", err)
	}
	if err := createFromTemplate(ctx, admin, name, tmpl); err != nil {
		t.Fatalf("pgtest: criar banco: %v", err)
	}

	dbURL := withDatabase(t, adminURL, name)
	t.Cleanup(func() { dropDatabase(adminURL, name) })

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("pgtest: pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return &DB{Pool: pool, URL: dbURL}
}

// templateLockKey identifica o advisory lock que serializa a criação do
// banco-modelo entre processos (cada pacote de teste roda em um processo).
const templateLockKey = 7_365_021_884

// templates guarda, por processo, os modelos já garantidos por URL administrativa.
var templates sync.Map

// ensureTemplate devolve o nome do banco-modelo com as migrations atuais,
// criando-o se ainda não existir.
func ensureTemplate(ctx context.Context, admin *pgx.Conn, adminURL string) (string, error) {
	if name, ok := templates.Load(adminURL); ok {
		return name.(string), nil
	}
	hash, err := migrationsHash()
	if err != nil {
		return "", err
	}
	name := "pgtest_tmpl_" + hash

	exists, err := databaseExists(ctx, admin, name)
	if err != nil {
		return "", err
	}
	if !exists {
		if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", templateLockKey); err != nil {
			return "", fmt.Errorf("lock: %w", err)
		}
		defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", templateLockKey) }()
		// Outro processo pode ter criado o modelo enquanto este esperava o lock.
		if exists, err = databaseExists(ctx, admin, name); err != nil {
			return "", err
		}
		if !exists {
			if err := buildTemplate(ctx, admin, adminURL, name); err != nil {
				return "", err
			}
		}
	}
	templates.Store(adminURL, name)
	return name, nil
}

// buildTemplate migra um banco temporário e só então o renomeia para o nome
// final: um processo interrompido no meio nunca deixa um modelo incompleto.
func buildTemplate(ctx context.Context, admin *pgx.Conn, adminURL, name string) error {
	building := name + "_building"
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{building}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("remover modelo incompleto: %w", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{building}.Sanitize()); err != nil {
		return fmt.Errorf("criar modelo: %w", err)
	}
	u, err := url.Parse(adminURL)
	if err != nil {
		return fmt.Errorf("URL inválida: %w", err)
	}
	u.Path = "/" + building
	if err := migrate(ctx, u.String()); err != nil {
		return err
	}
	if err := execWhileInUse(ctx, admin, "ALTER DATABASE "+pgx.Identifier{building}.Sanitize()+" RENAME TO "+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("renomear modelo: %w", err)
	}
	return nil
}

func createFromTemplate(ctx context.Context, admin *pgx.Conn, name, tmpl string) error {
	return execWhileInUse(ctx, admin, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE "+pgx.Identifier{tmpl}.Sanitize())
}

// execWhileInUse repete stmt enquanto o PostgreSQL o recusar porque outra sessão
// ainda está conectada ao banco envolvido (código 55006), o que só dura instantes:
// uma sessão recém-fechada ou outra cópia do mesmo modelo em andamento.
func execWhileInUse(ctx context.Context, admin *pgx.Conn, stmt string) error {
	for {
		_, err := admin.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "55006" {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func databaseExists(ctx context.Context, admin *pgx.Conn, name string) (bool, error) {
	var exists bool
	err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
	return exists, err
}

// migrationsHash resume o conteúdo das migrations embutidas.
func migrationsHash() (string, error) {
	h := sha256.New()
	err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(migrations.FS, path)
		if err != nil {
			return err
		}
		h.Write([]byte(path))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("ler migrations: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

func migrate(ctx context.Context, dbURL string) error {
	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		return fmt.Errorf("abrir banco: %w", err)
	}
	defer sqlDB.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		return fmt.Errorf("goose: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	return nil
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
