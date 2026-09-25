package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/feliphexavier/jungleGamingProjeto/internal/config"
)

func setValid(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5433/db")
	t.Setenv("OIDC_ISSUER", "http://localhost:8081/realms/wallet")
	t.Setenv("OIDC_JWKS_URL", "http://localhost:8081/realms/wallet/protocol/openid-connect/certs")
	t.Setenv("OIDC_AUDIENCE", "wallet-api")
	t.Setenv("HTTP_PORT", "8080")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("SQS_INBOUND_QUEUE", "wager-operations.fifo")
	t.Setenv("SQS_DLQ", "wager-operations-dlq.fifo")
	t.Setenv("SQS_EVENTS_QUEUE", "wallet-events.fifo")
}

func TestLoadValid(t *testing.T) {
	setValid(t)
	t.Setenv("SHUTDOWN_TIMEOUT", "5s")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.ShutdownTimeout != 5*time.Second || cfg.OIDC.Audience != "wallet-api" {
		t.Errorf("config = %+v", cfg)
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := map[string]func(t *testing.T){
		"sem DATABASE_URL":          func(t *testing.T) { t.Setenv("DATABASE_URL", "") },
		"sem OIDC_ISSUER":           func(t *testing.T) { t.Setenv("OIDC_ISSUER", "") },
		"porta inválida":            func(t *testing.T) { t.Setenv("HTTP_PORT", "abc") },
		"timeout inválido":          func(t *testing.T) { t.Setenv("SHUTDOWN_TIMEOUT", "-1s") },
		"log level inválido":        func(t *testing.T) { t.Setenv("LOG_LEVEL", "VERBOSE") },
		"sem fila de eventos":       func(t *testing.T) { t.Setenv("SQS_EVENTS_QUEUE", "") },
		"long polling acima de 20s": func(t *testing.T) { t.Setenv("SQS_WAIT_TIME", "30s") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			setValid(t)
			mutate(t)
			if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "configuração inválida") {
				t.Errorf("err = %v, want configuração inválida", err)
			}
		})
	}
}
