.PHONY: db-up db-down migrate migrate-down migrate-status

db-up:
	docker compose up -d --wait postgres

db-down:
	docker compose down

migrate:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate down

migrate-status:
	go run ./cmd/migrate status
