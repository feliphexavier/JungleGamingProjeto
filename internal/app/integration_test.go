package app_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/postgres"
	"github.com/feliphexavier/jungleGamingProjeto/internal/testsupport/pgtest"
)

var (
	internal  = app.Principal{Internal: true}
	providerA = app.Principal{ProviderID: "provider-a"}
	providerB = app.Principal{ProviderID: "provider-b"}
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	t     *testing.T
	ctx   context.Context
	db    *pgtest.DB
	svc   *app.Service
	clock *fakeClock
}

func setup(t *testing.T, opts ...app.Option) *env {
	t.Helper()
	db := pgtest.New(t)
	clock := &fakeClock{now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	svc := app.NewService(postgres.NewStore(db.Pool), append([]app.Option{app.WithClock(clock)}, opts...)...)
	return &env{t: t, ctx: context.Background(), db: db, svc: svc, clock: clock}
}

func (e *env) openWallet(amount string) *domain.Wallet {
	e.t.Helper()
	w, err := e.svc.OpenWallet(e.ctx, app.OpenWalletCommand{
		Principal: internal, PlayerID: uuid.NewString(), Amount: amount, Currency: "BRL",
	})
	if err != nil {
		e.t.Fatalf("OpenWallet: %v", err)
	}
	return w
}

func wager(w *domain.Wallet, kind, amount, externalID, ref string) app.SubmitWagerCommand {
	return app.SubmitWagerCommand{
		Principal:                      providerA,
		IdempotencyKey:                 "provider-a:" + externalID,
		ProviderID:                     "provider-a",
		ExternalTransactionID:          externalID,
		PlayerID:                       w.PlayerID().String(),
		WalletID:                       w.ID().String(),
		RoundID:                        "round-1",
		GameID:                         "fortune-chimp",
		Kind:                           kind,
		Amount:                         amount,
		Currency:                       "BRL",
		ReferenceExternalTransactionID: ref,
	}
}

func (e *env) submit(cmd app.SubmitWagerCommand) app.SubmitWagerResult {
	e.t.Helper()
	r, err := e.svc.SubmitWager(e.ctx, cmd)
	if err != nil {
		e.t.Fatalf("SubmitWager(%s %s): %v", cmd.Kind, cmd.ExternalTransactionID, err)
	}
	return r
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count: %v", err)
	}
	return n
}

// expectWallet confere saldo, versão e a reconciliação com o ledger.
func (e *env) expectWallet(id uuid.UUID, amount string, version int64) {
	e.t.Helper()
	w, err := e.svc.GetWallet(e.ctx, internal, id)
	if err != nil {
		e.t.Fatal(err)
	}
	if w.Balance().Amount() != amount || w.Version() != version {
		e.t.Errorf("carteira = %s v%d, want %s v%d", w.Balance().Amount(), w.Version(), amount, version)
	}
	rec, err := e.svc.Reconcile(e.ctx, internal, id)
	if err != nil {
		e.t.Fatal(err)
	}
	if !rec.Consistent {
		e.t.Errorf("reconciliação divergente: armazenado %s, ledger %s", rec.StoredBalance, rec.CalculatedBalance)
	}
}

// sha gera um hash no formato exigido pela inbox (SHA-256 em hex).
func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func status(r app.SubmitWagerResult) domain.Status { return r.Transaction.Status() }

func TestOpenWallet(t *testing.T) {
	e := setup(t)
	w := e.openWallet("1000.00")
	e.expectWallet(w.ID(), "1000.00", 1)

	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND kind = 'OPENING' AND status = 'PROCESSED'`, w.ID()); n != 1 {
		t.Errorf("OPENING = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'CREDIT'`, w.ID()); n != 1 {
		t.Errorf("lançamentos = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM outbox_events WHERE message_group_id = $1`, w.ID().String()); n != 2 {
		t.Errorf("eventos na outbox = %d, want 2", n)
	}

	// Mesmo jogador e moeda: conflito.
	_, err := e.svc.OpenWallet(e.ctx, app.OpenWalletCommand{
		Principal: internal, PlayerID: w.PlayerID().String(), Amount: "0.00", Currency: "BRL",
	})
	if !errors.Is(err, app.ErrWalletAlreadyExists) {
		t.Errorf("segunda carteira = %v, want ErrWalletAlreadyExists", err)
	}
}

func TestOpenWalletZeroBalance(t *testing.T) {
	e := setup(t)
	w := e.openWallet("0.00")
	e.expectWallet(w.ID(), "0.00", 1)
	for table, n := range map[string]int{
		"wager_transactions":    e.count(`SELECT count(*) FROM wager_transactions WHERE wallet_id = $1`, w.ID()),
		"wallet_ledger_entries": e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, w.ID()),
		"outbox_events":         e.count(`SELECT count(*) FROM outbox_events WHERE message_group_id = $1`, w.ID().String()),
	} {
		if n != 0 {
			t.Errorf("%s = %d, want 0 para saldo inicial zero", table, n)
		}
	}
}

func TestOpenWalletRequiresInternal(t *testing.T) {
	e := setup(t)
	_, err := e.svc.OpenWallet(e.ctx, app.OpenWalletCommand{
		Principal: providerA, PlayerID: uuid.NewString(), Amount: "10.00", Currency: "BRL",
	})
	if !errors.Is(err, app.ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
	if n := e.count(`SELECT count(*) FROM wallets`); n != 0 {
		t.Errorf("carteiras = %d, want 0", n)
	}
}

func TestBetAndReplayReturnsOriginalBalance(t *testing.T) {
	e := setup(t)
	w := e.openWallet("1000.00")

	first := e.submit(wager(w, "BET", "25.00", "bet-1", ""))
	if status(first) != domain.StatusProcessed || first.Replay {
		t.Fatalf("primeira: %s replay=%v", status(first), first.Replay)
	}
	e.submit(wager(w, "BET", "100.00", "bet-2", "")) // carteira muda depois

	replay := e.submit(wager(w, "BET", "25.00", "bet-1", ""))
	if !replay.Replay || replay.Transaction.ID() != first.Transaction.ID() {
		t.Fatalf("replay = %+v", replay)
	}
	if bal, _ := replay.Transaction.ResultBalance(); bal.Amount() != "975.00" {
		t.Errorf("saldo do replay = %s, want 975.00 (observado no processamento original)", bal)
	}
	e.expectWallet(w.ID(), "875.00", 3)
}

func TestIdempotencyConflicts(t *testing.T) {
	e := setup(t)
	w := e.openWallet("1000.00")
	e.submit(wager(w, "BET", "25.00", "bet-1", ""))

	sameKeyOtherContent := wager(w, "BET", "30.00", "bet-1", "")
	if _, err := e.svc.SubmitWager(e.ctx, sameKeyOtherContent); !errors.Is(err, app.ErrIdempotencyConflict) {
		t.Errorf("mesma chave, outro conteúdo = %v, want ErrIdempotencyConflict", err)
	}

	otherKey := wager(w, "BET", "25.00", "bet-1", "")
	otherKey.IdempotencyKey = "outra-chave"
	if _, err := e.svc.SubmitWager(e.ctx, otherKey); !errors.Is(err, app.ErrIdempotencyConflict) {
		t.Errorf("mesma operação, outra chave = %v, want ErrIdempotencyConflict", err)
	}
	e.expectWallet(w.ID(), "975.00", 2)
}

func TestSubmitValidation(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")

	tests := []struct {
		name   string
		mutate func(*app.SubmitWagerCommand)
		want   error
	}{
		{"provedor do token diferente do corpo", func(c *app.SubmitWagerCommand) { c.Principal = providerB }, app.ErrForbidden},
		{"sem provedor autenticado", func(c *app.SubmitWagerCommand) { c.Principal = app.Principal{} }, app.ErrForbidden},
		{"sem chave", func(c *app.SubmitWagerCommand) { c.IdempotencyKey = "" }, app.ErrInvalidInput},
		{"OPENING externo", func(c *app.SubmitWagerCommand) { c.Kind = "OPENING" }, domain.ErrOpeningNotAllowed},
		{"valor em notação científica", func(c *app.SubmitWagerCommand) { c.Amount = "1e3" }, domain.ErrInvalidAmount},
		{"BET zero", func(c *app.SubmitWagerCommand) { c.Amount = "0.00" }, domain.ErrNonPositiveAmount},
		{"carteira inexistente", func(c *app.SubmitWagerCommand) { c.WalletID = uuid.NewString() }, app.ErrWalletNotFound},
		{"walletId inválido", func(c *app.SubmitWagerCommand) { c.WalletID = "abc" }, app.ErrInvalidInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := wager(w, "BET", "10.00", "bet-"+uuid.NewString(), "")
			tt.mutate(&cmd)
			if _, err := e.svc.SubmitWager(e.ctx, cmd); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE kind <> 'OPENING'`); n != 0 {
		t.Errorf("transações gravadas = %d, want 0 (entradas inválidas não são persistidas)", n)
	}
	e.expectWallet(w.ID(), "100.00", 1)
}

// Cenário obrigatório: 100.00 e duas apostas de 80.00 ao mesmo tempo.
func TestConcurrentBetsOverBalance(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")

	results := make([]app.SubmitWagerResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = e.svc.SubmitWager(e.ctx, wager(w, "BET", "80.00", fmt.Sprintf("bet-%d", i), ""))
		}()
	}
	close(start)
	wg.Wait()

	counts := map[domain.Status]int{}
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("aposta %d: %v", i, errs[i])
		}
		counts[status(results[i])]++
		if status(results[i]) == domain.StatusRejected && results[i].Transaction.FailureCode() != domain.FailureInsufficientFunds {
			t.Errorf("código = %s", results[i].Transaction.FailureCode())
		}
	}
	if counts[domain.StatusProcessed] != 1 || counts[domain.StatusRejected] != 1 {
		t.Errorf("resultados = %v, want 1 PROCESSED e 1 REJECTED", counts)
	}
	e.expectWallet(w.ID(), "20.00", 2)
	if n := e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID()); n != 1 {
		t.Errorf("débitos = %d, want 1", n)
	}

	// Reenvios não mudam o resultado.
	for i := range 2 {
		r := e.submit(wager(w, "BET", "80.00", fmt.Sprintf("bet-%d", i), ""))
		if !r.Replay || status(r) != status(results[i]) {
			t.Errorf("reenvio %d: replay=%v status=%s", i, r.Replay, status(r))
		}
	}
	e.expectWallet(w.ID(), "20.00", 2)
}

// A mesma aposta enviada 50 vezes em paralelo gera um único débito.
func TestSameBet50TimesInParallel(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")

	const n = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	replays, fresh := 0, 0
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := e.svc.SubmitWager(e.ctx, wager(w, "BET", "10.00", "bet-1", ""))
			if err != nil {
				t.Errorf("SubmitWager: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if r.Replay {
				replays++
			} else {
				fresh++
			}
		}()
	}
	close(start)
	wg.Wait()

	if fresh != 1 || replays != n-1 {
		t.Errorf("novas = %d, replays = %d; want 1 e %d", fresh, replays, n-1)
	}
	if c := e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID()); c != 1 {
		t.Errorf("débitos = %d, want 1", c)
	}
	e.expectWallet(w.ID(), "90.00", 2)
}

// Carteiras diferentes avançam em paralelo: com a carteira A travada por outra
// transação, uma operação na carteira B termina normalmente.
func TestDifferentWalletsDoNotBlockEachOther(t *testing.T) {
	e := setup(t)
	a := e.openWallet("100.00")
	b := e.openWallet("100.00")

	lockTx, err := e.db.Pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(e.ctx)
	if _, err := lockTx.Exec(e.ctx, `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, a.ID()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	if _, err := e.svc.SubmitWager(ctx, wager(b, "BET", "10.00", "bet-b", "")); err != nil {
		t.Fatalf("carteira B bloqueada pela A: %v", err)
	}

	// A carteira A espera o lock e só termina depois de liberado.
	done := make(chan error, 1)
	go func() {
		_, err := e.svc.SubmitWager(e.ctx, wager(a, "BET", "10.00", "bet-a", ""))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("carteira A não esperou o lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := lockTx.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("carteira A após liberar: %v", err)
	}
	e.expectWallet(a.ID(), "90.00", 2)
	e.expectWallet(b.ID(), "90.00", 2)
}

func TestManyWalletsConcurrently(t *testing.T) {
	e := setup(t)
	const wallets, betsPerWallet = 10, 10
	ws := make([]*domain.Wallet, wallets)
	for i := range ws {
		ws[i] = e.openWallet("100.00")
	}

	var wg sync.WaitGroup
	for _, w := range ws {
		for j := range betsPerWallet {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := e.svc.SubmitWager(e.ctx, wager(w, "BET", "5.00", fmt.Sprintf("bet-%s-%d", w.ID(), j), "")); err != nil {
					t.Errorf("SubmitWager: %v", err)
				}
			}()
		}
	}
	wg.Wait()
	for _, w := range ws {
		e.expectWallet(w.ID(), "50.00", 1+betsPerWallet)
	}
}

func TestLossDoesNotMoveBalance(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")
	r := e.submit(wager(w, "LOSS", "0.00", "loss-1", ""))
	if status(r) != domain.StatusProcessed {
		t.Fatalf("status = %s", status(r))
	}
	e.expectWallet(w.ID(), "100.00", 1)
	if n := e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id = $1`, r.Transaction.ID()); n != 0 {
		t.Errorf("LOSS gerou %d lançamentos", n)
	}
	if n := e.count(`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'WagerTransactionProcessed'`, r.Transaction.ID()); n != 1 {
		t.Errorf("WagerTransactionProcessed = %d, want 1", n)
	}
}

func TestRefundThenRollbackSameBet(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")
	e.submit(wager(w, "BET", "40.00", "bet-1", ""))
	if r := e.submit(wager(w, "REFUND", "40.00", "refund-1", "bet-1")); status(r) != domain.StatusProcessed {
		t.Fatalf("REFUND: %s %s", status(r), r.Transaction.FailureCode())
	}
	r := e.submit(wager(w, "ROLLBACK", "40.00", "rb-1", "bet-1"))
	if status(r) != domain.StatusRejected || r.Transaction.FailureCode() != domain.FailureAlreadyReversed {
		t.Errorf("ROLLBACK após REFUND: %s %s", status(r), r.Transaction.FailureCode())
	}
	e.expectWallet(w.ID(), "100.00", 3)
}

func TestRollbackOfWinWithoutFunds(t *testing.T) {
	e := setup(t)
	w := e.openWallet("0.00")
	e.submit(wager(w, "WIN", "50.00", "win-1", ""))
	e.submit(wager(w, "BET", "40.00", "bet-1", ""))
	r := e.submit(wager(w, "ROLLBACK", "50.00", "rb-1", "win-1"))
	if status(r) != domain.StatusRejected || r.Transaction.FailureCode() != domain.FailureInsufficientFundsForReversal {
		t.Errorf("status %s código %s", status(r), r.Transaction.FailureCode())
	}
	e.expectWallet(w.ID(), "10.00", 3)
}

// ROLLBACK chega antes da BET: fica PENDING_REFERENCE e é resolvido depois.
func TestReversalBeforeReference(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")

	rb := e.submit(wager(w, "ROLLBACK", "40.00", "rb-1", "bet-1"))
	if status(rb) != domain.StatusPendingReference {
		t.Fatalf("status = %s, want PENDING_REFERENCE", status(rb))
	}
	if n := e.count(`SELECT count(*) FROM outbox_events WHERE event_type = 'WagerTransactionPendingReference'`); n != 1 {
		t.Errorf("eventos de pendência = %d, want 1", n)
	}

	// Replay de uma pendência devolve a pendência.
	if r := e.submit(wager(w, "ROLLBACK", "40.00", "rb-1", "bet-1")); !r.Replay || status(r) != domain.StatusPendingReference {
		t.Errorf("replay da pendência: %+v", r)
	}

	e.submit(wager(w, "BET", "40.00", "bet-1", ""))
	e.expectWallet(w.ID(), "60.00", 2)

	// A BET antecipou a próxima tentativa: o worker resolve sem esperar o backoff.
	n, err := e.svc.ResumePendingReferences(e.ctx, 10)
	if err != nil || n != 1 {
		t.Fatalf("ResumePendingReferences = %d, %v", n, err)
	}
	got, err := e.svc.GetTransaction(e.ctx, providerA, rb.Transaction.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() != domain.StatusProcessed {
		t.Errorf("ROLLBACK após referência = %s %s", got.Status(), got.FailureCode())
	}
	e.expectWallet(w.ID(), "100.00", 3)
}

func TestPendingReferenceExpires(t *testing.T) {
	policy := domain.ReferenceRetryPolicy{BaseDelay: time.Second, MaxDelay: time.Second, MaxAttempts: 2}
	e := setup(t, app.WithRetryPolicy(policy))
	w := e.openWallet("100.00")

	rb := e.submit(wager(w, "REFUND", "40.00", "refund-1", "bet-never"))
	if status(rb) != domain.StatusPendingReference {
		t.Fatalf("status = %s", status(rb))
	}

	// Antes do vencimento, nada é retomado.
	if n, err := e.svc.ResumePendingReferences(e.ctx, 10); err != nil || n != 0 {
		t.Fatalf("retomou antes do prazo: %d, %v", n, err)
	}
	for range 2 {
		e.clock.Advance(2 * time.Second)
		if _, err := e.svc.ResumePendingReferences(e.ctx, 10); err != nil {
			t.Fatal(err)
		}
	}

	got, err := e.svc.GetTransaction(e.ctx, providerA, rb.Transaction.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() != domain.StatusRejected || got.FailureCode() != domain.FailureReferenceNotFound {
		t.Errorf("após expirar: %s %s", got.Status(), got.FailureCode())
	}
	if n := e.count(`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'WagerTransactionRejected'`, got.ID()); n != 1 {
		t.Errorf("eventos de rejeição = %d, want 1", n)
	}
	e.expectWallet(w.ID(), "100.00", 1)
}

func TestSQSInboxDeduplication(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")

	msg := &app.InboxMessage{ConsumerName: "wager-consumer", MessageID: "msg-1", PayloadHash: sha("m1")}
	cmd := wager(w, "BET", "10.00", "bet-1", "")
	cmd.Inbox = msg

	first := e.submit(cmd)
	if first.Replay || status(first) != domain.StatusProcessed {
		t.Fatalf("primeira entrega: %+v", first)
	}

	// Reentrega da mesma mensagem: reconhecida pela inbox.
	again := e.submit(cmd)
	if !again.Replay || again.Transaction.ID() != first.Transaction.ID() {
		t.Errorf("reentrega: %+v", again)
	}

	// Mesmo messageId com outro conteúdo: conflito.
	tampered := cmd
	tampered.Inbox = &app.InboxMessage{ConsumerName: "wager-consumer", MessageID: "msg-1", PayloadHash: sha("m2")}
	if _, err := e.svc.SubmitWager(e.ctx, tampered); !errors.Is(err, app.ErrInboxConflict) {
		t.Errorf("messageId com outro hash = %v, want ErrInboxConflict", err)
	}

	// Mesma operação por HTTP (sem inbox): replay.
	cmd.Inbox = nil
	if r := e.submit(cmd); !r.Replay {
		t.Error("HTTP após SQS deveria ser replay")
	}

	// Nova mensagem SQS com a mesma operação: replay e inbox concluída.
	cmd.Inbox = &app.InboxMessage{ConsumerName: "wager-consumer", MessageID: "msg-2", PayloadHash: sha("m1")}
	if r := e.submit(cmd); !r.Replay {
		t.Error("nova mensagem com a mesma operação deveria ser replay")
	}
	if n := e.count(`SELECT count(*) FROM inbox_messages WHERE processed_at IS NOT NULL`); n != 2 {
		t.Errorf("inbox concluídas = %d, want 2", n)
	}
	e.expectWallet(w.ID(), "90.00", 2)
}

// HTTP e SQS disputando a mesma operação ao mesmo tempo.
func TestHTTPAndSQSConcurrentSameOperation(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cmd := wager(w, "BET", "10.00", "bet-1", "")
			if i%2 == 0 {
				cmd.Inbox = &app.InboxMessage{ConsumerName: "wager-consumer", MessageID: fmt.Sprintf("msg-%d", i), PayloadHash: sha("m1")}
			}
			if _, err := e.svc.SubmitWager(e.ctx, cmd); err != nil {
				t.Errorf("SubmitWager: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	e.expectWallet(w.ID(), "90.00", 2)
}

func TestProviderIsolation(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")
	r := e.submit(wager(w, "BET", "10.00", "bet-1", ""))

	if _, err := e.svc.GetTransaction(e.ctx, providerB, r.Transaction.ID()); !errors.Is(err, app.ErrTransactionNotFound) {
		t.Errorf("provider-b lendo transação de provider-a = %v, want ErrTransactionNotFound", err)
	}
	if _, err := e.svc.GetTransactionByExternalID(e.ctx, providerB, "provider-a", "bet-1"); !errors.Is(err, app.ErrForbidden) {
		t.Errorf("provider-b consultando provider-a = %v, want ErrForbidden", err)
	}
	if _, err := e.svc.GetTransactionByExternalID(e.ctx, providerB, "provider-b", "bet-1"); !errors.Is(err, app.ErrTransactionNotFound) {
		t.Errorf("provider-b com o mesmo id externo = %v, want ErrTransactionNotFound", err)
	}
	if got, err := e.svc.GetTransactionByExternalID(e.ctx, providerA, "provider-a", "bet-1"); err != nil || got.ID() != r.Transaction.ID() {
		t.Errorf("provider-a consultando a própria = %v", err)
	}
	if _, err := e.svc.GetWallet(e.ctx, providerA, w.ID()); !errors.Is(err, app.ErrForbidden) {
		t.Errorf("provedor lendo carteira = %v, want ErrForbidden", err)
	}
	if _, err := e.svc.Reconcile(e.ctx, providerA, w.ID()); !errors.Is(err, app.ErrForbidden) {
		t.Errorf("provedor reconciliando = %v, want ErrForbidden", err)
	}

	// provider-b não consegue reaplicar a operação de provider-a com a mesma chave.
	cmd := wager(w, "BET", "10.00", "bet-1", "")
	cmd.Principal = providerB
	cmd.ProviderID = "provider-b"
	cmd.IdempotencyKey = "provider-a:bet-1"
	res, err := e.svc.SubmitWager(e.ctx, cmd)
	if err != nil || res.Replay {
		t.Errorf("provider-b com a chave de provider-a: replay=%v err=%v (chave é por provedor)", res.Replay, err)
	}
}

func TestLedgerPagination(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")
	for i := range 4 {
		e.submit(wager(w, "BET", "1.00", fmt.Sprintf("bet-%d", i), ""))
	}

	var versions []int64
	cursor := ""
	pages := 0
	for {
		page, err := e.svc.ListLedger(e.ctx, internal, w.ID(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, entry := range page.Entries {
			versions = append(versions, entry.WalletVersion())
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if fmt.Sprint(versions) != "[1 2 3 4 5]" || pages != 3 {
		t.Errorf("versões = %v em %d páginas", versions, pages)
	}

	if _, err := e.svc.ListLedger(e.ctx, internal, w.ID(), "lixo", 2); !errors.Is(err, app.ErrInvalidInput) {
		t.Errorf("cursor inválido = %v", err)
	}
}

func TestReconcileReport(t *testing.T) {
	e := setup(t)
	w := e.openWallet("1000.00")
	e.submit(wager(w, "BET", "25.00", "bet-1", ""))

	rec, err := e.svc.Reconcile(e.ctx, internal, w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if rec.StoredBalance.Amount() != "975.00" || rec.CalculatedBalance.Amount() != "975.00" ||
		rec.Difference.Amount() != "0.00" || !rec.Consistent || rec.CheckedEntries != 2 {
		t.Errorf("reconciliação = %+v", rec)
	}
}
