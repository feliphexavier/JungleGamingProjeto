package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth/authtest"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/httpapi"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/postgres"
	"github.com/feliphexavier/jungleGamingProjeto/internal/testsupport/pgtest"
)

type apiEnv struct {
	t        *testing.T
	srv      *httptest.Server
	db       *pgtest.DB
	internal string
	provA    string
	provB    string
	idp      *authtest.Issuer
}

func newAPI(t *testing.T) *apiEnv {
	t.Helper()
	db := pgtest.New(t)
	idp := authtest.NewIssuer(t)
	verifier, err := auth.NewVerifier(context.Background(), idp.Config())
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewStore(db.Pool)
	checks := []httpapi.HealthCheck{{Name: "postgres", Check: store.Ping}}
	server := httpapi.NewServer(app.NewService(store), verifier, checks, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)
	return &apiEnv{
		t: t, srv: srv, db: db, idp: idp,
		internal: idp.InternalToken(t),
		provA:    idp.ProviderToken(t, "provider-a"),
		provB:    idp.ProviderToken(t, "provider-b"),
	}
}

type response struct {
	status int
	header http.Header
	body   map[string]any
}

func (e *apiEnv) do(method, path, token string, body any, headers ...string) response {
	e.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := response{status: res.StatusCode, header: res.Header}
	raw, _ := io.ReadAll(res.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			e.t.Fatalf("resposta não é JSON (%d): %s", res.StatusCode, raw)
		}
	}
	return out
}

func (e *apiEnv) openWallet(amount string) (walletID, playerID string) {
	e.t.Helper()
	playerID = uuid.NewString()
	r := e.do("POST", "/wallets", e.internal, map[string]any{
		"playerId": playerID, "initialBalance": map[string]string{"amount": amount, "currency": "BRL"},
	})
	if r.status != http.StatusCreated {
		e.t.Fatalf("POST /wallets = %d %v", r.status, r.body)
	}
	return r.body["id"].(string), playerID
}

func wagerBody(walletID, playerID, kind, amount, externalID, ref string) map[string]any {
	b := map[string]any{
		"providerId": "provider-a", "externalTransactionId": externalID,
		"playerId": playerID, "walletId": walletID, "roundId": "round-987", "gameId": "fortune-chimp",
		"kind": kind, "money": map[string]string{"amount": amount, "currency": "BRL"},
	}
	if ref != "" {
		b["referenceExternalTransactionId"] = ref
	}
	return b
}

func (e *apiEnv) submit(token, key string, body map[string]any) response {
	e.t.Helper()
	return e.do("POST", "/wagering/transactions", token, body, "Idempotency-Key", key)
}

func money(v any) string {
	m, _ := v.(map[string]any)
	return m["amount"].(string) + " " + m["currency"].(string)
}

func errCode(r response) string {
	e, _ := r.body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func TestWalletContract(t *testing.T) {
	e := newAPI(t)
	playerID := uuid.NewString()
	r := e.do("POST", "/wallets", e.internal, map[string]any{
		"playerId": playerID, "initialBalance": map[string]string{"amount": "1000.00", "currency": "BRL"},
	})
	if r.status != http.StatusCreated || r.body["playerId"] != playerID ||
		money(r.body["balance"]) != "1000.00 BRL" || r.body["version"].(float64) != 1 {
		t.Fatalf("POST /wallets = %d %v", r.status, r.body)
	}
	id := r.body["id"].(string)

	dup := e.do("POST", "/wallets", e.internal, map[string]any{
		"playerId": playerID, "initialBalance": map[string]string{"amount": "0.00", "currency": "BRL"},
	})
	if dup.status != http.StatusConflict || errCode(dup) != httpapi.CodeWalletAlreadyExists {
		t.Errorf("carteira duplicada = %d %v", dup.status, dup.body)
	}

	if g := e.do("GET", "/wallets/"+id, e.internal, nil); g.status != 200 || money(g.body["balance"]) != "1000.00 BRL" {
		t.Errorf("GET /wallets = %d %v", g.status, g.body)
	}
	if g := e.do("GET", "/wallets/"+uuid.NewString(), e.internal, nil); g.status != 404 || errCode(g) != httpapi.CodeWalletNotFound {
		t.Errorf("carteira inexistente = %d %v", g.status, g.body)
	}
	if g := e.do("GET", "/wallets/nao-e-uuid", e.internal, nil); g.status != 400 {
		t.Errorf("walletId inválido = %d", g.status)
	}
}

func TestWagerContract(t *testing.T) {
	e := newAPI(t)
	walletID, playerID := e.openWallet("1000.00")

	bet := wagerBody(walletID, playerID, "BET", "25.00", "transaction-123", "")
	r := e.submit(e.provA, "provider-a:transaction-123", bet)
	if r.status != http.StatusCreated || r.body["status"] != "PROCESSED" ||
		money(r.body["balance"]) != "975.00 BRL" || r.body["idempotentReplay"] != false {
		t.Fatalf("BET = %d %v", r.status, r.body)
	}
	txID := r.body["transactionId"].(string)

	// Replay: 200, mesmo resultado, saldo original.
	e.submit(e.provA, "provider-a:transaction-200", wagerBody(walletID, playerID, "BET", "100.00", "transaction-200", ""))
	replay := e.submit(e.provA, "provider-a:transaction-123", bet)
	if replay.status != http.StatusOK || replay.body["idempotentReplay"] != true ||
		replay.body["transactionId"] != txID || money(replay.body["balance"]) != "975.00 BRL" {
		t.Errorf("replay = %d %v", replay.status, replay.body)
	}

	// Chave reutilizada com outro conteúdo: 409.
	other := wagerBody(walletID, playerID, "BET", "26.00", "transaction-123", "")
	if c := e.submit(e.provA, "provider-a:transaction-123", other); c.status != http.StatusConflict || errCode(c) != httpapi.CodeIdempotencyConflict {
		t.Errorf("conflito = %d %v", c.status, c.body)
	}

	// Rejeição de negócio: 422 com failureCode.
	rej := e.submit(e.provA, "k-big", wagerBody(walletID, playerID, "BET", "5000.00", "big", ""))
	if rej.status != http.StatusUnprocessableEntity || rej.body["status"] != "REJECTED" || rej.body["failureCode"] != "INSUFFICIENT_FUNDS" {
		t.Errorf("rejeição = %d %v", rej.status, rej.body)
	}

	// Referência ainda inexistente: 202.
	pend := e.submit(e.provA, "k-rb", wagerBody(walletID, playerID, "ROLLBACK", "10.00", "rb-1", "bet-futura"))
	if pend.status != http.StatusAccepted || pend.body["status"] != "PENDING_REFERENCE" {
		t.Errorf("pendência = %d %v", pend.status, pend.body)
	}
	if _, has := pend.body["balance"]; has {
		t.Error("pendência não deve informar saldo")
	}

	// Consultas.
	g := e.do("GET", "/wagering/transactions/"+txID, e.provA, nil)
	if g.status != 200 || g.body["kind"] != "BET" || g.body["externalTransactionId"] != "transaction-123" {
		t.Errorf("GET transação = %d %v", g.status, g.body)
	}
	g = e.do("GET", "/providers/provider-a/wagering/transactions/transaction-123", e.provA, nil)
	if g.status != 200 || g.body["transactionId"] != txID {
		t.Errorf("GET por id externo = %d %v", g.status, g.body)
	}
}

func TestWagerInputErrors(t *testing.T) {
	e := newAPI(t)
	walletID, playerID := e.openWallet("100.00")
	base := func() map[string]any { return wagerBody(walletID, playerID, "BET", "10.00", "x-1", "") }

	tests := []struct {
		name   string
		key    string
		body   any
		status int
		code   string
	}{
		{"sem Idempotency-Key", "", base(), 400, httpapi.CodeInvalidInput},
		{"amount numérico", "k1", `{"providerId":"provider-a","externalTransactionId":"x-1","playerId":"` + playerID +
			`","walletId":"` + walletID + `","roundId":"r","gameId":"g","kind":"BET","money":{"amount":10.00,"currency":"BRL"}}`, 400, httpapi.CodeInvalidInput},
		{"escala excedente", "k2", func() any {
			b := base()
			b["money"] = map[string]string{"amount": "10.001", "currency": "BRL"}
			return b
		}(), 400, httpapi.CodeInvalidInput},
		{"negativo", "k3", func() any {
			b := base()
			b["money"] = map[string]string{"amount": "-1.00", "currency": "BRL"}
			return b
		}(), 400, httpapi.CodeInvalidInput},
		{"OPENING externo", "k4", func() any { b := base(); b["kind"] = "OPENING"; return b }(), 400, httpapi.CodeInvalidInput},
		{"campo desconhecido", "k5", func() any { b := base(); b["extra"] = 1; return b }(), 400, httpapi.CodeInvalidInput},
		{"JSON quebrado", "k6", `{"providerId":`, 400, httpapi.CodeInvalidInput},
		{"dois objetos", "k7", `{} {}`, 400, httpapi.CodeInvalidInput},
		{"providerId de outro provedor", "k8", func() any { b := base(); b["providerId"] = "provider-b"; return b }(), 403, httpapi.CodeForbidden},
		{"carteira inexistente", "k9", func() any { b := base(); b["walletId"] = uuid.NewString(); return b }(), 404, httpapi.CodeWalletNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r response
			if tt.key == "" {
				r = e.do("POST", "/wagering/transactions", e.provA, tt.body)
			} else {
				r = e.do("POST", "/wagering/transactions", e.provA, tt.body, "Idempotency-Key", tt.key)
			}
			if r.status != tt.status || errCode(r) != tt.code {
				t.Errorf("= %d %v, want %d %s", r.status, r.body, tt.status, tt.code)
			}
		})
	}

	var n int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM wager_transactions WHERE kind <> 'OPENING'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("transações gravadas = %d (%v), want 0", n, err)
	}
}

// Sem autenticação válida, nenhuma rota de negócio responde nem tem efeito.
func TestAuthenticationRequired(t *testing.T) {
	e := newAPI(t)
	walletID, playerID := e.openWallet("100.00")

	tokens := map[string]string{
		"sem token":      "",
		"token inválido": "abc.def.ghi",
		"token expirado": e.idp.ExpiredToken(t, "provider-a"),
	}

	routes := []struct{ method, path string }{
		{"POST", "/wallets"},
		{"GET", "/wallets/" + walletID},
		{"GET", "/wallets/" + walletID + "/ledger"},
		{"POST", "/wallets/" + walletID + "/reconciliation"},
		{"POST", "/wagering/transactions"},
		{"GET", "/wagering/transactions/" + uuid.NewString()},
		{"GET", "/providers/provider-a/wagering/transactions/x"},
	}
	for name, token := range tokens {
		for _, rt := range routes {
			t.Run(name+" "+rt.method+" "+rt.path, func(t *testing.T) {
				r := e.do(rt.method, rt.path, token, wagerBody(walletID, playerID, "BET", "10.00", "unauth", ""),
					"Idempotency-Key", "unauth")
				if r.status != http.StatusUnauthorized || errCode(r) != httpapi.CodeUnauthenticated {
					t.Errorf("= %d %v, want 401", r.status, r.body)
				}
				if !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Bearer") {
					t.Error("sem WWW-Authenticate")
				}
			})
		}
	}

	// Nenhum efeito financeiro.
	var n int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM wager_transactions WHERE kind <> 'OPENING'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("transações gravadas sem autenticação = %d (%v)", n, err)
	}
	if g := e.do("GET", "/wallets/"+walletID, e.internal, nil); money(g.body["balance"]) != "100.00 BRL" {
		t.Errorf("saldo alterado: %v", g.body)
	}
}

func TestAuthorization(t *testing.T) {
	e := newAPI(t)
	walletID, playerID := e.openWallet("100.00")
	r := e.submit(e.provA, "provider-a:bet-1", wagerBody(walletID, playerID, "BET", "10.00", "bet-1", ""))
	txID := r.body["transactionId"].(string)

	tests := []struct {
		name, method, path, token string
		status                    int
	}{
		{"provedor abre carteira", "POST", "/wallets", e.provA, 403},
		{"provedor lê carteira", "GET", "/wallets/" + walletID, e.provA, 403},
		{"provedor lê ledger", "GET", "/wallets/" + walletID + "/ledger", e.provA, 403},
		{"provedor reconcilia", "POST", "/wallets/" + walletID + "/reconciliation", e.provA, 403},
		{"outro provedor lê transação", "GET", "/wagering/transactions/" + txID, e.provB, 404},
		{"outro provedor consulta por id externo", "GET", "/providers/provider-a/wagering/transactions/bet-1", e.provB, 403},
		{"outro provedor consulta o próprio namespace", "GET", "/providers/provider-b/wagering/transactions/bet-1", e.provB, 404},
		{"interno lê transação", "GET", "/wagering/transactions/" + txID, e.internal, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]any{"playerId": uuid.NewString(), "initialBalance": map[string]string{"amount": "1.00", "currency": "BRL"}}
			if res := e.do(tt.method, tt.path, tt.token, body); res.status != tt.status {
				t.Errorf("= %d %v, want %d", res.status, res.body, tt.status)
			}
		})
	}

	// Provedor B não consegue reenviar a operação de A (nem como replay).
	bBody := wagerBody(walletID, playerID, "BET", "10.00", "bet-1", "")
	if res := e.submit(e.provB, "provider-a:bet-1", bBody); res.status != 403 {
		t.Errorf("provider-b enviando operação de provider-a = %d %v", res.status, res.body)
	}
	// Serviço interno não envia operações de provedor.
	if res := e.submit(e.internal, "provider-a:bet-1", bBody); res.status != 403 {
		t.Errorf("serviço interno enviando operação = %d %v", res.status, res.body)
	}
}

func TestLedgerAndReconciliation(t *testing.T) {
	e := newAPI(t)
	walletID, playerID := e.openWallet("1000.00")
	e.submit(e.provA, "k1", wagerBody(walletID, playerID, "BET", "25.00", "t1", ""))

	rec := e.do("POST", "/wallets/"+walletID+"/reconciliation", e.internal, nil)
	if rec.status != 200 || money(rec.body["storedBalance"]) != "975.00 BRL" ||
		money(rec.body["calculatedBalance"]) != "975.00 BRL" || money(rec.body["difference"]) != "0.00 BRL" ||
		rec.body["consistent"] != true || rec.body["checkedEntries"].(float64) != 2 {
		t.Errorf("reconciliação = %d %v", rec.status, rec.body)
	}

	page := e.do("GET", "/wallets/"+walletID+"/ledger?limit=1", e.internal, nil)
	items, _ := page.body["items"].([]any)
	if page.status != 200 || len(items) != 1 || page.body["nextCursor"] == nil {
		t.Fatalf("página 1 = %d %v", page.status, page.body)
	}
	next := e.do("GET", "/wallets/"+walletID+"/ledger?limit=1&cursor="+page.body["nextCursor"].(string), e.internal, nil)
	items, _ = next.body["items"].([]any)
	if next.status != 200 || len(items) != 1 || next.body["nextCursor"] != nil {
		t.Errorf("página 2 = %d %v", next.status, next.body)
	}
	if first := items[0].(map[string]any); first["direction"] != "DEBIT" || money(first["balanceAfter"]) != "975.00 BRL" {
		t.Errorf("lançamento = %v", first)
	}
	if bad := e.do("GET", "/wallets/"+walletID+"/ledger?limit=0", e.internal, nil); bad.status != 400 {
		t.Errorf("limit=0 = %d", bad.status)
	}
}

func TestHealthIsPublic(t *testing.T) {
	e := newAPI(t)
	if r := e.do("GET", "/health/live", "", nil); r.status != 200 || r.body["status"] != "UP" {
		t.Errorf("live = %d %v", r.status, r.body)
	}
	r := e.do("GET", "/health/ready", "", nil)
	checks, _ := r.body["checks"].(map[string]any)
	if r.status != 200 || checks["postgres"] != "UP" {
		t.Errorf("ready = %d %v", r.status, r.body)
	}
}

func TestReadinessReportsDependencyDown(t *testing.T) {
	down := httpapi.HealthCheck{Name: "sqs", Check: func(context.Context) error { return errors.New("fora do ar") }}
	server := httpapi.NewServer(nil, nil, []httpapi.HealthCheck{down}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("ready com dependência fora = %d, want 503", res.StatusCode)
	}
}

func TestCorrelationID(t *testing.T) {
	e := newAPI(t)
	r := e.do("GET", "/health/live", "", nil, httpapi.CorrelationHeader, "abc-123")
	if r.header.Get(httpapi.CorrelationHeader) != "abc-123" {
		t.Errorf("correlation propagado = %q", r.header.Get(httpapi.CorrelationHeader))
	}
	r = e.do("GET", "/health/live", "", nil, httpapi.CorrelationHeader, "inválido com espaço")
	if got := r.header.Get(httpapi.CorrelationHeader); got == "" || strings.Contains(got, " ") {
		t.Errorf("correlation inválido deveria ser substituído, veio %q", got)
	}
}
