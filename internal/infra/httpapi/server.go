// Package httpapi expõe os casos de uso por HTTP, com roteamento chi.
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
)

// HealthCheck verifica uma dependência para o readiness.
type HealthCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

// Server contém os handlers HTTP.
type Server struct {
	svc    *app.Service
	auth   Authenticator
	checks []HealthCheck
	log    *slog.Logger
}

func NewServer(svc *app.Service, authenticator Authenticator, checks []HealthCheck, log *slog.Logger) *Server {
	return &Server{svc: svc, auth: authenticator, checks: checks, log: log}
}

// Handler monta as rotas. Health checks são públicos; todas as rotas de
// negócio exigem bearer token válido.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(withCorrelation, s.recoverer, s.accessLog)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{
			Code: CodeNotFound, Message: fmt.Sprintf("rota inexistente: %s %s", r.Method, truncate(r.URL.Path, 200)), CorrelationID: correlationID(r.Context()),
		}})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: errorDetail{
			Code: CodeNotFound, Message: fmt.Sprintf("método não permitido: %s %s", r.Method, truncate(r.URL.Path, 200)), CorrelationID: correlationID(r.Context()),
		}})
	})

	r.Get("/health/live", s.live)
	r.Get("/health/ready", s.ready)

	// Documentação (pública): especificação OpenAPI e Swagger UI.
	r.Get("/openapi.yaml", s.openAPISpec)
	r.Get("/docs", s.docs)

	r.Group(func(r chi.Router) {
		r.Use(s.authenticate)

		// A permissão é verificada antes de ler o corpo: quem não tem acesso
		// recebe 403 sem que a entrada seja processada. Os casos de uso repetem
		// a verificação.
		r.Group(func(r chi.Router) {
			r.Use(s.require(func(p app.Principal) bool { return p.Internal }, "restrito ao serviço interno"))
			r.Post("/wallets", s.openWallet)
			r.Get("/wallets/{walletId}", s.getWallet)
			r.Get("/wallets/{walletId}/ledger", s.listLedger)
			r.Post("/wallets/{walletId}/reconciliation", s.reconcile)
		})

		r.With(s.require(func(p app.Principal) bool { return p.ProviderID != "" }, "restrito a provedores")).
			Post("/wagering/transactions", s.submitWager)

		r.Group(func(r chi.Router) {
			r.Use(s.require(func(p app.Principal) bool { return p.Internal || p.ProviderID != "" }, "sem permissão de consulta"))
			r.Get("/wagering/transactions/{transactionId}", s.getTransaction)
			r.Get("/providers/{providerId}/wagering/transactions/{externalTransactionId}", s.getTransactionByExternalID)
		})
	})
	return r
}

// require recusa com 403 quem não atende à permissão da rota.
func (s *Server) require(allowed func(app.Principal) bool, reason string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allowed(principal(r.Context())) {
				s.writeError(w, r, &forbiddenError{reason: reason})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type forbiddenError struct{ reason string }

func (e *forbiddenError) Error() string { return app.ErrForbidden.Error() + ": " + e.reason }
func (e *forbiddenError) Unwrap() error { return app.ErrForbidden }

func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "UP"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	status := http.StatusOK
	results := make(map[string]string, len(s.checks))
	for _, c := range s.checks {
		if err := c.Check(ctx); err != nil {
			status = http.StatusServiceUnavailable
			results[c.Name] = "DOWN"
			s.log.WarnContext(ctx, "readiness: dependência indisponível", slog.String("dependency", c.Name), slog.String("error", err.Error()))
			continue
		}
		results[c.Name] = "UP"
	}
	overall := "UP"
	if status != http.StatusOK {
		overall = "DOWN"
	}
	writeJSON(w, status, map[string]any{"status": overall, "checks": results})
}

func (s *Server) openWallet(w http.ResponseWriter, r *http.Request) {
	var req openWalletRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	wallet, err := s.svc.OpenWallet(r.Context(), app.OpenWalletCommand{
		Principal:     principal(r.Context()),
		PlayerID:      req.PlayerID,
		Amount:        req.InitialBalance.Amount,
		Currency:      req.InitialBalance.Currency,
		CorrelationID: correlationID(r.Context()),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/wallets/"+wallet.ID().String())
	writeJSON(w, http.StatusCreated, toWallet(wallet))
}

func (s *Server) getWallet(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(chi.URLParam(r, "walletId"), "walletId")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	wallet, err := s.svc.GetWallet(r.Context(), principal(r.Context()), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toWallet(wallet))
}

func (s *Server) listLedger(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(chi.URLParam(r, "walletId"), "walletId")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit <= 0 {
			s.writeError(w, r, invalid("limit deve ser inteiro positivo"))
			return
		}
	}
	page, err := s.svc.ListLedger(r.Context(), principal(r.Context()), id, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	resp := ledgerResponse{WalletID: id, Items: make([]ledgerEntryResponse, 0, len(page.Entries)), NextCursor: page.NextCursor}
	for _, e := range page.Entries {
		resp.Items = append(resp.Items, ledgerEntryResponse{
			ID: e.ID(), TransactionID: e.TransactionID(), Direction: e.Direction(), Money: e.Amount(),
			BalanceBefore: e.BalanceBefore(), BalanceAfter: e.BalanceAfter(),
			WalletVersion: e.WalletVersion(), CreatedAt: e.CreatedAt(),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(chi.URLParam(r, "walletId"), "walletId")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rec, err := s.svc.Reconcile(r.Context(), principal(r.Context()), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !rec.Consistent {
		s.log.ErrorContext(r.Context(), "reconciliação divergente",
			slog.String("walletId", id.String()),
			slog.String("difference", rec.Difference.Amount()),
			slog.String("correlationId", correlationID(r.Context())))
	}
	writeJSON(w, http.StatusOK, reconciliationResponse{
		WalletID: rec.WalletID, StoredBalance: rec.StoredBalance, CalculatedBalance: rec.CalculatedBalance,
		Difference: rec.Difference, Consistent: rec.Consistent, CheckedEntries: rec.CheckedEntries,
	})
}

// IdempotencyHeader é obrigatório em POST /wagering/transactions.
const IdempotencyHeader = "Idempotency-Key"

func (s *Server) submitWager(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get(IdempotencyHeader)
	if key == "" {
		s.writeError(w, r, invalid("header Idempotency-Key é obrigatório"))
		return
	}
	var req wagerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.svc.SubmitWager(r.Context(), app.SubmitWagerCommand{
		Principal:                      principal(r.Context()),
		IdempotencyKey:                 key,
		ProviderID:                     req.ProviderID,
		ExternalTransactionID:          req.ExternalTransactionID,
		PlayerID:                       req.PlayerID,
		WalletID:                       req.WalletID,
		RoundID:                        req.RoundID,
		GameID:                         req.GameID,
		Kind:                           req.Kind,
		Amount:                         req.Money.Amount,
		Currency:                       req.Money.Currency,
		ReferenceExternalTransactionID: req.ReferenceExternalTransactionID,
		CorrelationID:                  correlationID(r.Context()),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, wagerStatus(result), toWagerResponse(result))
}

// wagerStatus: 201 processada agora, 200 replay de processada, 202 aguardando
// referência, 422 rejeitada ou com falha permanente (resultado terminal: não
// adianta repetir).
func wagerStatus(r app.SubmitWagerResult) int {
	switch r.Transaction.Status() {
	case domain.StatusProcessed:
		if r.Replay {
			return http.StatusOK
		}
		return http.StatusCreated
	case domain.StatusPendingReference, domain.StatusPending:
		return http.StatusAccepted
	default:
		return http.StatusUnprocessableEntity
	}
}

func (s *Server) getTransaction(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(chi.URLParam(r, "transactionId"), "transactionId")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	tx, err := s.svc.GetTransaction(r.Context(), principal(r.Context()), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransaction(tx))
}

func (s *Server) getTransactionByExternalID(w http.ResponseWriter, r *http.Request) {
	tx, err := s.svc.GetTransactionByExternalID(r.Context(), principal(r.Context()),
		chi.URLParam(r, "providerId"), chi.URLParam(r, "externalTransactionId"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransaction(tx))
}

func invalid(msg string) error {
	return &invalidInputError{msg: msg}
}

type invalidInputError struct{ msg string }

func (e *invalidInputError) Error() string { return app.ErrInvalidInput.Error() + ": " + e.msg }
func (e *invalidInputError) Unwrap() error { return app.ErrInvalidInput }
