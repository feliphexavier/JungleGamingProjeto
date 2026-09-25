package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth"
)

// Códigos de erro do contrato HTTP (campo error.code).
const (
	CodeInvalidInput        = "INVALID_INPUT"
	CodeUnauthenticated     = "UNAUTHENTICATED"
	CodeForbidden           = "FORBIDDEN"
	CodeWalletNotFound      = "WALLET_NOT_FOUND"
	CodeTransactionNotFound = "TRANSACTION_NOT_FOUND"
	CodeNotFound            = "NOT_FOUND"
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	CodeWalletAlreadyExists = "WALLET_ALREADY_EXISTS"
	CodeUnavailable         = "SERVICE_UNAVAILABLE"
	CodeInternal            = "INTERNAL_ERROR"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlationId,omitempty"`
}

// classify traduz um erro da aplicação em status HTTP e código estável.
func classify(err error) (status int, code string) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return http.StatusUnauthorized, CodeUnauthenticated
	case errors.Is(err, app.ErrForbidden):
		return http.StatusForbidden, CodeForbidden
	case errors.Is(err, app.ErrInvalidInput):
		return http.StatusBadRequest, CodeInvalidInput
	case errors.Is(err, app.ErrWalletNotFound):
		return http.StatusNotFound, CodeWalletNotFound
	case errors.Is(err, app.ErrTransactionNotFound):
		return http.StatusNotFound, CodeTransactionNotFound
	case errors.Is(err, app.ErrIdempotencyConflict):
		return http.StatusConflict, CodeIdempotencyConflict
	case errors.Is(err, app.ErrWalletAlreadyExists):
		return http.StatusConflict, CodeWalletAlreadyExists
	case errors.Is(err, app.ErrUnavailable), errors.Is(err, app.ErrConcurrentUpdate), errors.Is(err, app.ErrDuplicate):
		return http.StatusServiceUnavailable, CodeUnavailable
	default:
		return http.StatusInternalServerError, CodeInternal
	}
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := classify(err)
	msg := err.Error()
	switch status {
	case http.StatusInternalServerError:
		// Detalhes internos só no log.
		s.log.ErrorContext(r.Context(), "erro interno", slog.String("error", err.Error()))
		msg = "erro interno"
	case http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Bearer realm="wallet-api"`)
		msg = "credenciais ausentes ou inválidas"
	case http.StatusServiceUnavailable:
		w.Header().Set("Retry-After", "1")
		s.log.WarnContext(r.Context(), "indisponibilidade transitória", slog.String("error", err.Error()))
		msg = "serviço temporariamente indisponível; tente novamente"
	}
	writeJSON(w, status, errorBody{Error: errorDetail{
		Code: code, Message: msg, CorrelationID: correlationID(r.Context()),
	}})
}
