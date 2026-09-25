// Package config lê e valida a configuração a partir de variáveis de ambiente.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth"
)

type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	OIDC            auth.Config
	ShutdownTimeout time.Duration
	LogLevel        slog.Level

	// Worker de referências pendentes.
	PendingInterval  time.Duration
	PendingBatchSize int
}

// Load lê o ambiente. Qualquer valor ausente ou inválido impede a subida.
func Load() (Config, error) {
	var errs []error
	get := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			return v
		}
		return def
	}
	duration := func(key, def string) time.Duration {
		d, err := time.ParseDuration(get(key, def))
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: duração inválida", key))
		}
		return d
	}
	positive := func(key, def string) int {
		n, err := strconv.Atoi(get(key, def))
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("%s: inteiro positivo esperado", key))
		}
		return n
	}

	port := get("HTTP_PORT", "8080")
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		errs = append(errs, errors.New("HTTP_PORT inválida"))
	}

	cfg := Config{
		HTTPAddr:    ":" + port,
		DatabaseURL: get("DATABASE_URL", ""),
		OIDC: auth.Config{
			Issuer:   get("OIDC_ISSUER", ""),
			JWKSURL:  get("OIDC_JWKS_URL", ""),
			Audience: get("OIDC_AUDIENCE", "wallet-api"),
		},
		ShutdownTimeout:  duration("SHUTDOWN_TIMEOUT", "20s"),
		PendingInterval:  duration("PENDING_REFERENCE_INTERVAL", "1s"),
		PendingBatchSize: positive("PENDING_REFERENCE_BATCH", "100"),
	}

	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL é obrigatória"))
	}
	if err := cfg.OIDC.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(strings.ToUpper(get("LOG_LEVEL", "INFO")))); err != nil {
		errs = append(errs, errors.New("LOG_LEVEL inválido"))
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("configuração inválida: %w", errors.Join(errs...))
	}
	return cfg, nil
}
