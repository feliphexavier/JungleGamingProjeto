package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
)

// Authenticator valida o bearer token e devolve a identidade.
type Authenticator interface {
	Authenticate(ctx context.Context, rawToken string) (app.Principal, error)
}

type ctxKey int

const (
	correlationKey ctxKey = iota
	principalKey
)

// CorrelationHeader propaga o id de correlação entre serviços.
const CorrelationHeader = "X-Correlation-ID"

var correlationPattern = regexp.MustCompile(`^[A-Za-z0-9._:\-]{1,128}$`)

// withCorrelation aceita o X-Correlation-ID recebido (se bem formado) ou gera
// um novo, e o devolve na resposta.
func withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(CorrelationHeader)
		if !correlationPattern.MatchString(id) {
			id = uuid.NewString()
		}
		w.Header().Set(CorrelationHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), correlationKey, id)))
	})
}

func correlationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey).(string)
	return id
}

func principal(ctx context.Context) app.Principal {
	p, _ := ctx.Value(principalKey).(app.Principal)
	return p
}

// authenticate exige um bearer token válido. Sem ele, a requisição termina em
// 401 antes de chegar a qualquer caso de uso.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok {
			token = ""
		}
		p, err := s.auth.Authenticate(r.Context(), strings.TrimSpace(token))
		if err != nil {
			s.log.InfoContext(r.Context(), "autenticação recusada",
				slog.String("correlationId", correlationID(r.Context())), slog.String("reason", err.Error()))
			s.writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// accessLog registra uma linha JSON por requisição, sem corpo nem credenciais.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := chi.RouteContext(r.Context()).RoutePattern()
		attrs := []any{
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.Int("status", rec.status),
			slog.Duration("duration", time.Since(start)),
			slog.String("correlationId", correlationID(r.Context())),
		}
		if route == "" {
			// Sem rota correspondente: registra o caminho pedido (sem query string).
			attrs = append(attrs, slog.String("path", truncate(r.URL.Path, 200)))
		}
		s.log.InfoContext(r.Context(), "http request", attrs...)
	})
}

// recoverer transforma um panic em 500 com corpo JSON, sem derrubar o processo.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.ErrorContext(r.Context(), "panic no handler", slog.Any("panic", rec),
					slog.String("correlationId", correlationID(r.Context())))
				writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{
					Code: CodeInternal, Message: "erro interno", CorrelationID: correlationID(r.Context()),
				}})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
