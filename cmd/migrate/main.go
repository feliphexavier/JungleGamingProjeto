package main

import (
	"context"
	"database/sql"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/feliphexavier/jungleGamingProjeto/migrations"
)

// uso: go run ./cmd/migrate [up|down|status|reset|version] (padrão: up)
func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://apostas:apostas@localhost:5433/apostas?sslmode=disable"
	}

	command, args := "up", []string{}
	if len(os.Args) > 1 {
		command, args = os.Args[1], os.Args[2:]
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("abrir conexão: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		log.Fatal(err)
	}

	if err := goose.RunContext(context.Background(), command, db, ".", args...); err != nil {
		log.Fatalf("goose %s: %v", command, err)
	}
}
