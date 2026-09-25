package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var meta = EventMeta{CorrelationID: "corr-1", CausationID: "msg-1"}

func decodeEvent(t *testing.T, ev Event) map[string]any {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func eventTypes(events []Event) []string {
	var types []string
	for _, e := range events {
		types = append(types, e.Type())
	}
	return types
}

func TestEventsForProcessedBet(t *testing.T) {
	s := newScenario(t, "100.00")
	bet := s.op(KindBet, "25.00", "bet-1", "")
	r := s.process(bet, nil, false)

	events, err := TransactionEvents(bet, r.LedgerEntry, meta, uuid.New)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(eventTypes(events), ","); got != "WagerTransactionProcessed,WalletBalanceChanged" {
		t.Fatalf("eventos = %s", got)
	}
	if events[0].ID() == events[1].ID() {
		t.Error("eventIds devem ser distintos")
	}

	env := decodeEvent(t, events[1])
	for _, key := range []string{"eventId", "eventType", "aggregateId", "correlationId", "causationId", "occurredAt", "version", "data"} {
		if _, ok := env[key]; !ok {
			t.Errorf("envelope sem %s", key)
		}
	}
	if env["version"].(float64) != 1 || env["aggregateId"] != s.wallet.ID().String() {
		t.Errorf("envelope = %v", env)
	}
	data := env["data"].(map[string]any)
	for _, key := range []string{"walletId", "transactionId", "direction", "money", "balanceBefore", "balanceAfter", "walletVersion"} {
		if _, ok := data[key]; !ok {
			t.Errorf("WalletBalanceChanged sem %s", key)
		}
	}
	if data["balanceAfter"].(map[string]any)["amount"] != "75.00" || data["direction"] != "DEBIT" {
		t.Errorf("payload = %v", data)
	}
	if !strings.HasSuffix(env["occurredAt"].(string), "Z") {
		t.Errorf("occurredAt não está em UTC: %v", env["occurredAt"])
	}
}

func TestEventsForLoss(t *testing.T) {
	s := newScenario(t, "100.00")
	loss := s.op(KindLoss, "0.00", "loss-1", "")
	r := s.process(loss, nil, false)
	events, err := TransactionEvents(loss, r.LedgerEntry, meta, uuid.New)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(eventTypes(events), ","); got != "WagerTransactionProcessed" {
		t.Errorf("LOSS deve gerar só WagerTransactionProcessed, gerou %s", got)
	}
}

func TestEventsForRejection(t *testing.T) {
	s := newScenario(t, "10.00")
	bet := s.op(KindBet, "80.00", "bet-1", "")
	s.process(bet, nil, false)
	events, err := TransactionEvents(bet, nil, meta, uuid.New)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type() != EventWagerTransactionRejected {
		t.Fatalf("eventos = %v", eventTypes(events))
	}
	data := decodeEvent(t, events[0])["data"].(map[string]any)
	if data["failureCode"] != "INSUFFICIENT_FUNDS" {
		t.Errorf("failureCode = %v", data["failureCode"])
	}
}

func TestEventsForPendingReferenceOnlyOnce(t *testing.T) {
	s := newScenario(t, "100.00")
	rb := s.op(KindRollback, "10.00", "rb-1", "bet-1")

	s.process(rb, nil, false)
	events, err := TransactionEvents(rb, nil, meta, uuid.New)
	if err != nil || len(events) != 1 || events[0].Type() != EventWagerTransactionPendingReference {
		t.Fatalf("primeira espera: %v %v", eventTypes(events), err)
	}

	s.process(rb, nil, false)
	events, err = TransactionEvents(rb, nil, meta, uuid.New)
	if err != nil || len(events) != 0 {
		t.Errorf("espera repetida não deve gerar evento: %v %v", eventTypes(events), err)
	}
}

func TestEventsForOpening(t *testing.T) {
	r, err := OpenWallet(OpenWalletParams{
		WalletID: uuid.New(), PlayerID: uuid.New(), OpeningID: uuid.New(), LedgerEntryID: uuid.New(),
		InitialBalance: mustMoney(t, "1000.00", "BRL"), Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := TransactionEvents(r.Opening, r.LedgerEntry, EventMeta{CorrelationID: "corr"}, uuid.New)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(eventTypes(events), ","); got != "WagerTransactionProcessed,WalletBalanceChanged" {
		t.Fatalf("eventos = %s", got)
	}
	env := decodeEvent(t, events[0])
	data := env["data"].(map[string]any)
	for _, key := range []string{"providerId", "externalTransactionId", "roundId", "gameId"} {
		if _, ok := data[key]; ok {
			t.Errorf("OPENING não deve ter %s", key)
		}
	}
	if _, ok := env["causationId"]; ok {
		t.Error("causationId vazio deve ser omitido")
	}
	changed := decodeEvent(t, events[1])["data"].(map[string]any)
	if changed["walletVersion"].(float64) != 1 {
		t.Errorf("versão na abertura = %v, want 1", changed["walletVersion"])
	}
}

func TestEventRequiresCorrelationID(t *testing.T) {
	s := newScenario(t, "100.00")
	bet := s.processed(KindBet, "1.00", "bet-1", "", nil)
	if _, err := TransactionEvents(bet, nil, EventMeta{}, uuid.New); err == nil {
		t.Error("esperava erro sem correlationId")
	}
}

func TestPayloadHash(t *testing.T) {
	base := PayloadFields{
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		PlayerID:              uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"),
		WalletID:              uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37"),
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  KindBet,
		Money:                 mustMoney(t, "25.00", "BRL"),
	}
	h1, err := PayloadHash(base)
	if err != nil {
		t.Fatal(err)
	}
	if !payloadHashPattern.MatchString(h1) {
		t.Errorf("hash fora do formato: %s", h1)
	}

	// Determinístico.
	if h2, _ := PayloadHash(base); h1 != h2 {
		t.Error("hash não determinístico")
	}

	// O hash é exatamente o SHA-256 deste JSON canônico. Fixar o texto protege
	// contra mudanças acidentais no algoritmo, que quebrariam a idempotência de
	// operações já gravadas.
	wantJSON := `{"externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"BET",` +
		`"money":{"amount":"25.00","currency":"BRL"},"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",` +
		`"providerId":"provider-a","roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`
	sum := sha256.Sum256([]byte(wantJSON))
	if want := hex.EncodeToString(sum[:]); h1 != want {
		t.Errorf("PayloadHash = %s, want SHA-256 de %s = %s", h1, wantJSON, want)
	}

	// Qualquer campo de negócio diferente muda o hash.
	variants := map[string]func(*PayloadFields){
		"provider":   func(f *PayloadFields) { f.ProviderID = "provider-b" },
		"externalId": func(f *PayloadFields) { f.ExternalTransactionID = "transaction-124" },
		"player":     func(f *PayloadFields) { f.PlayerID = uuid.New() },
		"wallet":     func(f *PayloadFields) { f.WalletID = uuid.New() },
		"round":      func(f *PayloadFields) { f.RoundID = "round-988" },
		"game":       func(f *PayloadFields) { f.GameID = "other" },
		"kind":       func(f *PayloadFields) { f.Kind = KindWin },
		"amount":     func(f *PayloadFields) { f.Money = mustMoney(t, "25.01", "BRL") },
		"currency":   func(f *PayloadFields) { f.Money = mustMinor(t, 2500, usd) },
		"reference":  func(f *PayloadFields) { f.ReferenceExternalTransactionID = "bet-1" },
	}
	for name, mutate := range variants {
		f := base
		mutate(&f)
		if h, _ := PayloadHash(f); h == h1 {
			t.Errorf("mudar %s não alterou o hash", name)
		}
	}

	if _, err := PayloadHash(PayloadFields{}); err == nil {
		t.Error("esperava erro com money não inicializado")
	}
}

func TestPayloadHashNoHTMLEscaping(t *testing.T) {
	f := PayloadFields{
		ProviderID: "a<b>&c", ExternalTransactionID: "x", PlayerID: uuid.New(), WalletID: uuid.New(),
		RoundID: "r", GameID: "g", Kind: KindBet, Money: mustMoney(t, "1.00", "BRL"),
	}
	data, err := canonicalJSON(map[string]any{"providerId": f.ProviderID})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"providerId":"a<b>&c"}` {
		t.Errorf("escape de HTML aplicado: %s", data)
	}
}
