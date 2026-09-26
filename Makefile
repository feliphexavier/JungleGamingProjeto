# Atalhos opcionais; os comandos equivalentes estão no README.
.PHONY: up down clean test test-race vet fmt migrate migrate-status

TEST_ENV = TEST_DATABASE_URL="postgres://apostas:apostas@localhost:5433/postgres?sslmode=disable" \
	TEST_KEYCLOAK_URL="http://127.0.0.1:8081" \
	TEST_SQS_ENDPOINT="http://localhost:4566" \
	TEST_API_URLS="http://localhost:8080,http://localhost:8082,http://localhost:8083"

up:
	docker compose up --build -d --wait

down:
	docker compose down

clean:
	docker compose down -v

test:
	$(TEST_ENV) go test ./...

test-race:
	$(TEST_ENV) go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

migrate:
	go run ./cmd/migrate up

migrate-status:
	go run ./cmd/migrate status
