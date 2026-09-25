package sqs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/google/uuid"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth/authtest"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/postgres"
	sqsinfra "github.com/feliphexavier/jungleGamingProjeto/internal/infra/sqs"
	"github.com/feliphexavier/jungleGamingProjeto/internal/testsupport/pgtest"
	"github.com/feliphexavier/jungleGamingProjeto/internal/testsupport/sqstest"
)

var discard = slog.New(slog.NewJSONHandler(io.Discard, nil))

type env struct {
	t        *testing.T
	ctx      context.Context
	db       *pgtest.DB
	store    *postgres.Store
	svc      *app.Service
	sqs      *sqstest.SQS
	idp      *authtest.Issuer
	consumer *sqsinfra.Consumer
}

func setup(t *testing.T) *env {
	t.Helper()
	q := sqstest.New(t)
	db := pgtest.New(t)
	idp := authtest.NewIssuer(t)
	verifier, err := auth.NewVerifier(context.Background(), idp.Config())
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewStore(db.Pool)
	svc := app.NewService(store)
	consumer := sqsinfra.NewConsumer(q.Client, verifier, svc, sqsinfra.ConsumerConfig{
		Queues: q.Queues, WaitTime: time.Second, MaxMessages: 10, ProcessTimeout: 10 * time.Second,
	}, discard)
	return &env{t: t, ctx: context.Background(), db: db, store: store, svc: svc, sqs: q, idp: idp, consumer: consumer}
}

func (e *env) openWallet(amount string) *domain.Wallet {
	e.t.Helper()
	w, err := e.svc.OpenWallet(e.ctx, app.OpenWalletCommand{
		Principal: app.Principal{Internal: true}, PlayerID: uuid.NewString(), Amount: amount, Currency: "BRL",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

// message monta o envelope de entrada. amount é inserido como está no JSON,
// para permitir testar um número sem aspas.
func message(messageID string, w *domain.Wallet, provider, externalID, kind, amountJSON string) string {
	return fmt.Sprintf(`{
		"messageId": %q,
		"type": "WagerOperationRequested",
		"occurredAt": "2026-09-25T12:00:00Z",
		"data": {
			"idempotencyKey": %q,
			"providerId": %q,
			"externalTransactionId": %q,
			"playerId": %q,
			"walletId": %q,
			"roundId": "round-1",
			"gameId": "fortune-chimp",
			"kind": %q,
			"money": {"amount": %s, "currency": "BRL"}
		}
	}`, messageID, provider+":"+externalID, provider, externalID, w.PlayerID(), w.ID(), kind, amountJSON)
}

// pollUntilEmpty processa até a fila de entrada não devolver mensagens.
func (e *env) pollUntilEmpty() {
	e.t.Helper()
	for i := 0; i < 20; i++ {
		n, err := e.consumer.Poll(e.ctx)
		if err != nil {
			e.t.Fatalf("Poll: %v", err)
		}
		if n == 0 {
			return
		}
	}
}

func (e *env) balance(id uuid.UUID) string {
	e.t.Helper()
	w, err := e.svc.GetWallet(e.ctx, app.Principal{Internal: true}, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return w.Balance().String()
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestConsumerProcessesAndDeduplicates(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")
	tokA := e.idp.ProviderToken(t, "provider-a")
	body := message("msg-1", w, "provider-a", "bet-1", "BET", `"30.00"`)

	// A mesma mensagem entregue duas vezes (dedup ids diferentes no SQS, mesmo
	// messageId no envelope): a inbox garante um único débito.
	e.sqs.Send(t, e.sqs.Queues.Inbound, body, w.ID().String(), "d1", tokA)
	e.sqs.Send(t, e.sqs.Queues.Inbound, body, w.ID().String(), "d2", tokA)
	// A mesma operação em outra mensagem: replay pela idempotência.
	e.sqs.Send(t, e.sqs.Queues.Inbound, message("msg-2", w, "provider-a", "bet-1", "BET", `"30.00"`), w.ID().String(), "d3", tokA)
	e.pollUntilEmpty()

	if got := e.balance(w.ID()); got != "70.00 BRL" {
		t.Errorf("saldo = %s, want 70.00 BRL", got)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID()); n != 1 {
		t.Errorf("débitos = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM inbox_messages WHERE processed_at IS NOT NULL`); n != 2 {
		t.Errorf("inbox concluídas = %d, want 2", n)
	}
	if left := e.sqs.Drain(t, e.sqs.Queues.Inbound); len(left) != 0 {
		t.Errorf("%d mensagens não apagadas", len(left))
	}
	if dlq := e.sqs.Drain(t, e.sqs.Queues.DLQ); len(dlq) != 0 {
		t.Errorf("DLQ = %d mensagens, want 0", len(dlq))
	}
}

func TestConsumerPermanentFailuresGoToDLQ(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")
	tokA := e.idp.ProviderToken(t, "provider-a")
	tokB := e.idp.ProviderToken(t, "provider-b")

	// Primeiro uma mensagem válida, para o conflito de messageId abaixo.
	e.sqs.Send(t, e.sqs.Queues.Inbound, message("ok-1", w, "provider-a", "bet-ok", "BET", `"10.00"`), "g-ok", "ok", tokA)
	e.pollUntilEmpty()

	cases := []struct {
		name, body, token, code string
	}{
		{"sem token", message("m-1", w, "provider-a", "bet-1", "BET", `"10.00"`), "", "UNAUTHENTICATED"},
		{"token expirado", message("m-2", w, "provider-a", "bet-2", "BET", `"10.00"`), e.idp.ExpiredToken(t, "provider-a"), "UNAUTHENTICATED"},
		{"token de outro provedor", message("m-3", w, "provider-a", "bet-3", "BET", `"10.00"`), tokB, "FORBIDDEN"},
		{"amount numérico", message("m-4", w, "provider-a", "bet-4", "BET", `10.00`), tokA, "INVALID_INPUT"},
		{"OPENING externo", message("m-5", w, "provider-a", "bet-5", "OPENING", `"10.00"`), tokA, "INVALID_INPUT"},
		{"messageId reutilizado com outro conteúdo", message("ok-1", w, "provider-a", "bet-6", "BET", `"10.00"`), tokA, "IDEMPOTENCY_CONFLICT"},
		{"envelope inválido", `{"messageId":"m-7","type":"Outro","data":{}}`, tokA, "INVALID_INPUT"},
	}
	for i, c := range cases {
		e.sqs.Send(t, e.sqs.Queues.Inbound, c.body, fmt.Sprintf("g-%d", i), fmt.Sprintf("bad-%d", i), c.token)
	}
	e.pollUntilEmpty()

	dlq := e.sqs.Drain(t, e.sqs.Queues.DLQ)
	codes := map[string]int{}
	for _, m := range dlq {
		if _, leaked := m.MessageAttributes[sqsinfra.AuthorizationAttribute]; leaked {
			t.Error("token copiado para a DLQ")
		}
		codes[aws.ToString(m.MessageAttributes["failureCode"].StringValue)]++
	}
	want := map[string]int{}
	for _, c := range cases {
		want[c.code]++
	}
	if fmt.Sprint(codes) != fmt.Sprint(want) {
		t.Errorf("códigos na DLQ = %v, want %v", codes, want)
	}
	if left := e.sqs.Drain(t, e.sqs.Queues.Inbound); len(left) != 0 {
		t.Errorf("%d mensagens ficaram na fila de entrada", len(left))
	}
	// Nenhuma falha teve efeito financeiro: só a aposta válida.
	if got := e.balance(w.ID()); got != "90.00 BRL" {
		t.Errorf("saldo = %s, want 90.00 BRL", got)
	}
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE provider_id IS NOT NULL`); n != 1 {
		t.Errorf("transações externas = %d, want 1", n)
	}
}

// Duas apostas de 80.00 sobre 100.00 recebidas por SQS e consumidas por três
// consumidores concorrentes: uma processada, uma rejeitada.
func TestConcurrentConsumers(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")
	tokA := e.idp.ProviderToken(t, "provider-a")
	verifier, _ := auth.NewVerifier(e.ctx, e.idp.Config())

	// Grupos diferentes para que o SQS entregue as duas em paralelo.
	e.sqs.Send(t, e.sqs.Queues.Inbound, message("c-1", w, "provider-a", "bet-1", "BET", `"80.00"`), "g1", "c1", tokA)
	e.sqs.Send(t, e.sqs.Queues.Inbound, message("c-2", w, "provider-a", "bet-2", "BET", `"80.00"`), "g2", "c2", tokA)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		c := sqsinfra.NewConsumer(e.sqs.Client, verifier, app.NewService(postgres.NewStore(e.db.Pool)), sqsinfra.ConsumerConfig{
			Queues: e.sqs.Queues, WaitTime: time.Second, MaxMessages: 1, ProcessTimeout: 10 * time.Second,
		}, discard)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				_, _ = c.Poll(e.ctx)
			}
		}()
	}
	wg.Wait()
	e.pollUntilEmpty()

	if got := e.balance(w.ID()); got != "20.00 BRL" {
		t.Errorf("saldo = %s, want 20.00 BRL", got)
	}
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND status = 'PROCESSED' AND kind = 'BET'`, w.ID()); n != 1 {
		t.Errorf("processadas = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND status = 'REJECTED'`, w.ID()); n != 1 {
		t.Errorf("rejeitadas = %d, want 1", n)
	}
}

type envelope struct {
	EventID     string `json:"eventId"`
	EventType   string `json:"eventType"`
	AggregateID string `json:"aggregateId"`
}

// Três publicadores concorrentes: cada evento chega à fila, na ordem de
// gravação dentro de cada carteira, e a outbox fica toda publicada.
func TestOutboxRelayPublishesInOrder(t *testing.T) {
	e := setup(t)
	wallets := []*domain.Wallet{e.openWallet("1000.00"), e.openWallet("1000.00"), e.openWallet("1000.00")}
	tokA := app.Principal{ProviderID: "provider-a"}
	for i := 0; i < 5; i++ {
		for _, w := range wallets {
			ext := fmt.Sprintf("bet-%s-%d", w.ID(), i)
			_, err := e.svc.SubmitWager(e.ctx, app.SubmitWagerCommand{
				Principal: tokA, IdempotencyKey: ext, ProviderID: "provider-a", ExternalTransactionID: ext,
				PlayerID: w.PlayerID().String(), WalletID: w.ID().String(), RoundID: "r", GameID: "g",
				Kind: "BET", Amount: "1.00", Currency: "BRL",
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	total := e.count(`SELECT count(*) FROM outbox_events`)

	pub := sqsinfra.NewPublisher(e.sqs.Client, e.sqs.Queues)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		relay := app.NewOutboxRelay(e.store, pub, app.OutboxRelayConfig{
			Owner: fmt.Sprintf("relay-%d", i), BatchSize: 4, Lease: 30 * time.Second, MaxBackoff: time.Second,
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50 && e.count(`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) > 0; j++ {
				if _, err := relay.PublishBatch(e.ctx); err != nil {
					t.Errorf("PublishBatch: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	if n := e.count(`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`); n != 0 {
		t.Fatalf("%d eventos não publicados", n)
	}

	// Ordem esperada por carteira: seq na outbox.
	rows, err := e.db.Pool.Query(e.ctx, `SELECT id::text, message_group_id FROM outbox_events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{}
	for rows.Next() {
		var id, group string
		if err := rows.Scan(&id, &group); err != nil {
			t.Fatal(err)
		}
		want[group] = append(want[group], id)
	}

	got := map[string][]string{}
	seen := map[string]bool{}
	for _, m := range e.sqs.Drain(t, e.sqs.Queues.Events) {
		var env envelope
		if err := json.Unmarshal([]byte(aws.ToString(m.Body)), &env); err != nil {
			t.Fatal(err)
		}
		if seen[env.EventID] {
			continue // reenvio com o mesmo eventId é permitido (entrega pelo menos uma vez)
		}
		seen[env.EventID] = true
		group := m.Attributes["MessageGroupId"]
		got[group] = append(got[group], env.EventID)
	}
	if len(seen) != total {
		t.Errorf("eventos distintos recebidos = %d, want %d", len(seen), total)
	}
	for group, ids := range want {
		if strings.Join(got[group], ",") != strings.Join(ids, ",") {
			t.Errorf("carteira %s: ordem recebida difere da gravada\n got %v\nwant %v", group, got[group], ids)
		}
	}
}
