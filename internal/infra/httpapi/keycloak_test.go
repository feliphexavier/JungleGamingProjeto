package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/httpapi"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/postgres"
	"github.com/feliphexavier/jungleGamingProjeto/internal/testsupport/kctest"
	"github.com/feliphexavier/jungleGamingProjeto/internal/testsupport/pgtest"
)

// Fluxos autenticados de ponta a ponta com tokens emitidos pelo Keycloak real
// (clients provisionados em deploy/keycloak/wallet-realm.json).
func TestWithRealKeycloak(t *testing.T) {
	kc := kctest.New(t)
	db := pgtest.New(t)

	verifier, err := auth.NewVerifier(context.Background(), kc.Config())
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewStore(db.Pool)
	server := httpapi.NewServer(app.NewService(store), verifier, nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)

	e := &apiEnv{
		t: t, srv: srv, db: db,
		internal: kc.Token(t, "wallet-internal", "wallet-internal-secret"),
		provA:    kc.Token(t, "provider-a", "provider-a-secret"),
		provB:    kc.Token(t, "provider-b", "provider-b-secret"),
	}
	untrusted := kc.Token(t, "untrusted-client", "untrusted-client-secret")

	t.Run("credencial inválida não obtém token", func(t *testing.T) {
		_, status, err := kc.RequestToken("provider-a", "segredo-errado")
		if err != nil || status != http.StatusUnauthorized {
			t.Errorf("Keycloak com segredo errado = %d, %v; want 401", status, err)
		}
	})

	walletID, playerID := e.openWallet("100.00")

	t.Run("provedor processa aposta", func(t *testing.T) {
		r := e.submit(e.provA, "provider-a:kc-bet-1", wagerBody(walletID, playerID, "BET", "10.00", "kc-bet-1", ""))
		if r.status != http.StatusCreated || money(r.body["balance"]) != "90.00 BRL" {
			t.Fatalf("aposta com token real = %d %v", r.status, r.body)
		}
		txID := r.body["transactionId"].(string)

		if g := e.do("GET", "/wagering/transactions/"+txID, e.provB, nil); g.status != http.StatusNotFound {
			t.Errorf("provider-b lendo transação de provider-a = %d", g.status)
		}
		if g := e.do("GET", "/providers/provider-a/wagering/transactions/kc-bet-1", e.provB, nil); g.status != http.StatusForbidden {
			t.Errorf("provider-b consultando provider-a = %d", g.status)
		}
		// Replay por outro provedor não é aceito.
		if r := e.submit(e.provB, "provider-a:kc-bet-1", wagerBody(walletID, playerID, "BET", "10.00", "kc-bet-1", "")); r.status != http.StatusForbidden {
			t.Errorf("replay por provider-b = %d", r.status)
		}
	})

	t.Run("client sem permissões", func(t *testing.T) {
		checks := []struct{ method, path string }{
			{"POST", "/wallets"},
			{"GET", "/wallets/" + walletID},
			{"POST", "/wallets/" + walletID + "/reconciliation"},
			{"POST", "/wagering/transactions"},
		}
		for _, c := range checks {
			r := e.do(c.method, c.path, untrusted, wagerBody(walletID, playerID, "BET", "10.00", "kc-x", ""), "Idempotency-Key", "kc-x")
			if r.status != http.StatusForbidden {
				t.Errorf("%s %s com client sem permissão = %d %v", c.method, c.path, r.status, r.body)
			}
		}
	})

	t.Run("token adulterado", func(t *testing.T) {
		parts := strings.Split(e.provA, ".")
		tampered := parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2]))
		if r := e.do("GET", "/wagering/transactions/"+uuid.NewString(), tampered, nil); r.status != http.StatusUnauthorized {
			t.Errorf("token com assinatura adulterada = %d", r.status)
		}
	})

	// Nenhum acesso não autorizado teve efeito financeiro.
	if g := e.do("GET", "/wallets/"+walletID, e.internal, nil); money(g.body["balance"]) != "90.00 BRL" {
		t.Errorf("saldo final = %v, want 90.00", g.body)
	}
}
