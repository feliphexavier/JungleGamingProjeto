package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// scenario monta uma carteira e cria operações do mesmo provedor, jogador e rodada.
type scenario struct {
	t      *testing.T
	wallet *Wallet
}

func newScenario(t *testing.T, balance string) *scenario {
	return &scenario{t: t, wallet: newTestWallet(t, balance)}
}

func (s *scenario) op(kind Kind, amount, externalID, ref string) *WagerTransaction {
	s.t.Helper()
	tx, err := NewExternalTransaction(ExternalTransactionParams{
		ID:                             uuid.New(),
		ProviderID:                     "provider-a",
		ExternalTransactionID:          externalID,
		IdempotencyKey:                 "provider-a:" + externalID,
		PayloadHash:                    testHash,
		PlayerID:                       s.wallet.PlayerID(),
		WalletID:                       s.wallet.ID(),
		RoundID:                        "round-1",
		GameID:                         "game-1",
		Kind:                           kind,
		Money:                          mustMoney(s.t, amount, "BRL"),
		ReferenceExternalTransactionID: ref,
		Now:                            t0,
	})
	if err != nil {
		s.t.Fatalf("NewExternalTransaction: %v", err)
	}
	return tx
}

func (s *scenario) process(tx, ref *WagerTransaction, reversed bool) ProcessResult {
	s.t.Helper()
	r, err := ProcessWager(ProcessInput{
		Transaction:       tx,
		Wallet:            s.wallet,
		Reference:         ref,
		ReferenceReversed: reversed,
		LedgerEntryID:     uuid.New(),
		RetryPolicy:       DefaultReferenceRetryPolicy,
		Now:               t0.Add(time.Minute),
	})
	if err != nil {
		s.t.Fatalf("ProcessWager(%s): %v", tx.Kind(), err)
	}
	return r
}

// processed cria e processa uma operação que deve ser aceita.
func (s *scenario) processed(kind Kind, amount, externalID, ref string, refTx *WagerTransaction) *WagerTransaction {
	s.t.Helper()
	tx := s.op(kind, amount, externalID, ref)
	if r := s.process(tx, refTx, false); r.Outcome != OutcomeProcessed {
		s.t.Fatalf("%s %s: outcome %s (%s)", kind, externalID, r.Outcome, tx.FailureCode())
	}
	return tx
}

func (s *scenario) expectBalance(amount string, version int64) {
	s.t.Helper()
	if s.wallet.Balance().Amount() != amount || s.wallet.Version() != version {
		s.t.Errorf("carteira = %s v%d, want %s v%d", s.wallet.Balance().Amount(), s.wallet.Version(), amount, version)
	}
}

func expectRejected(t *testing.T, r ProcessResult, tx *WagerTransaction, code FailureCode) {
	t.Helper()
	if r.Outcome != OutcomeRejected || tx.Status() != StatusRejected || tx.FailureCode() != code {
		t.Errorf("outcome %s status %s código %s, want REJECTED %s", r.Outcome, tx.Status(), tx.FailureCode(), code)
	}
	if r.LedgerEntry != nil {
		t.Error("rejeição não deve gerar lançamento")
	}
}

func TestProcessBet(t *testing.T) {
	s := newScenario(t, "1000.00")
	tx := s.op(KindBet, "25.00", "bet-1", "")
	r := s.process(tx, nil, false)

	if r.Outcome != OutcomeProcessed || tx.Status() != StatusProcessed {
		t.Fatalf("outcome %s status %s", r.Outcome, tx.Status())
	}
	if r.LedgerEntry == nil || r.LedgerEntry.Direction() != DirectionDebit || r.LedgerEntry.Amount().Amount() != "25.00" {
		t.Errorf("lançamento incorreto: %+v", r.LedgerEntry)
	}
	s.expectBalance("975.00", 2)
	if bal, _ := tx.ResultBalance(); bal.Amount() != "975.00" || tx.ResultWalletVersion() != 2 {
		t.Errorf("resultado gravado = %s v%d", bal, tx.ResultWalletVersion())
	}
}

// Cenário obrigatório no nível do domínio: 100.00 e duas apostas de 80.00.
func TestProcessTwoBetsOverBalance(t *testing.T) {
	s := newScenario(t, "100.00")
	first := s.op(KindBet, "80.00", "bet-1", "")
	second := s.op(KindBet, "80.00", "bet-2", "")

	if r := s.process(first, nil, false); r.Outcome != OutcomeProcessed {
		t.Fatalf("primeira aposta: %s", r.Outcome)
	}
	r := s.process(second, nil, false)
	expectRejected(t, r, second, FailureInsufficientFunds)
	s.expectBalance("20.00", 2)

	// O saldo observado na rejeição fica gravado para replays.
	if bal, ok := second.ResultBalance(); !ok || bal.Amount() != "20.00" {
		t.Errorf("saldo na rejeição = %s", bal)
	}
}

func TestProcessWin(t *testing.T) {
	s := newScenario(t, "100.00")
	tx := s.op(KindWin, "50.00", "win-1", "")
	r := s.process(tx, nil, false)
	if r.Outcome != OutcomeProcessed || r.LedgerEntry.Direction() != DirectionCredit {
		t.Fatalf("outcome %s", r.Outcome)
	}
	s.expectBalance("150.00", 2)
}

func TestProcessWinWithBetReference(t *testing.T) {
	s := newScenario(t, "100.00")
	bet := s.processed(KindBet, "10.00", "bet-1", "", nil)
	win := s.processed(KindWin, "30.00", "win-1", "bet-1", bet)
	if win.ReferenceTransactionID() != bet.ID() {
		t.Error("referência interna não resolvida")
	}
	s.expectBalance("120.00", 3)
}

func TestProcessLoss(t *testing.T) {
	s := newScenario(t, "100.00")
	tx := s.op(KindLoss, "0.00", "loss-1", "")
	r := s.process(tx, nil, false)
	if r.Outcome != OutcomeProcessed || tx.Status() != StatusProcessed {
		t.Fatalf("outcome %s", r.Outcome)
	}
	if r.LedgerEntry != nil {
		t.Error("LOSS não deve gerar lançamento")
	}
	s.expectBalance("100.00", 1) // versão não muda
	if bal, _ := tx.ResultBalance(); bal.Amount() != "100.00" || tx.ResultWalletVersion() != 1 {
		t.Errorf("resultado = %s v%d", bal, tx.ResultWalletVersion())
	}
}

func TestProcessRefund(t *testing.T) {
	s := newScenario(t, "100.00")
	bet := s.processed(KindBet, "40.00", "bet-1", "", nil)
	refund := s.processed(KindRefund, "40.00", "refund-1", "bet-1", bet)
	s.expectBalance("100.00", 3)
	if refund.ReferenceTransactionID() != bet.ID() {
		t.Error("referência interna não resolvida")
	}
}

func TestProcessRollbackOfEachKind(t *testing.T) {
	t.Run("BET credita", func(t *testing.T) {
		s := newScenario(t, "100.00")
		bet := s.processed(KindBet, "40.00", "bet-1", "", nil)
		s.processed(KindRollback, "40.00", "rb-1", "bet-1", bet)
		s.expectBalance("100.00", 3)
	})
	t.Run("WIN debita", func(t *testing.T) {
		s := newScenario(t, "100.00")
		win := s.processed(KindWin, "30.00", "win-1", "", nil)
		s.processed(KindRollback, "30.00", "rb-1", "win-1", win)
		s.expectBalance("100.00", 3)
	})
	t.Run("REFUND debita", func(t *testing.T) {
		s := newScenario(t, "100.00")
		bet := s.processed(KindBet, "40.00", "bet-1", "", nil)
		refund := s.processed(KindRefund, "40.00", "refund-1", "bet-1", bet)
		s.processed(KindRollback, "40.00", "rb-1", "refund-1", refund)
		s.expectBalance("60.00", 4) // a aposta volta a valer
	})
}

func TestRollbackWithoutFundsHasDistinctCode(t *testing.T) {
	s := newScenario(t, "0.00")
	win := s.processed(KindWin, "50.00", "win-1", "", nil)
	s.processed(KindBet, "30.00", "bet-1", "", nil) // saldo 20.00

	rb := s.op(KindRollback, "50.00", "rb-1", "win-1")
	r := s.process(rb, win, false)
	expectRejected(t, r, rb, FailureInsufficientFundsForReversal)
	s.expectBalance("20.00", 3)
}

func TestReversalRejections(t *testing.T) {
	type setup struct {
		s   *scenario
		ref *WagerTransaction
	}
	betSetup := func(t *testing.T) setup {
		s := newScenario(t, "100.00")
		return setup{s, s.processed(KindBet, "40.00", "bet-1", "", nil)}
	}

	tests := []struct {
		name     string
		kind     Kind
		amount   string
		reversed bool
		prepare  func(t *testing.T) setup
		mutate   func(ref *WagerTransaction)
		want     FailureCode
	}{
		{name: "REFUND com valor diferente", kind: KindRefund, amount: "10.00", prepare: betSetup, want: FailureReferenceAmountMismatch},
		{name: "ROLLBACK com valor diferente", kind: KindRollback, amount: "50.00", prepare: betSetup, want: FailureReferenceAmountMismatch},
		{name: "REFUND de BET já revertida", kind: KindRefund, amount: "40.00", reversed: true, prepare: betSetup, want: FailureAlreadyReversed},
		{name: "ROLLBACK de BET já reembolsada", kind: KindRollback, amount: "40.00", reversed: true, prepare: betSetup, want: FailureAlreadyReversed},
		{
			name: "REFUND de WIN", kind: KindRefund, amount: "30.00",
			prepare: func(t *testing.T) setup {
				s := newScenario(t, "100.00")
				return setup{s, s.processed(KindWin, "30.00", "bet-1", "", nil)}
			},
			want: FailureInvalidReferenceKind,
		},
		{
			name: "ROLLBACK de LOSS", kind: KindRollback, amount: "0.01",
			prepare: func(t *testing.T) setup {
				s := newScenario(t, "100.00")
				return setup{s, s.processed(KindLoss, "0.00", "bet-1", "", nil)}
			},
			want: FailureInvalidReferenceKind,
		},
		{
			name: "referência de outra rodada", kind: KindRefund, amount: "40.00", prepare: betSetup,
			mutate: func(ref *WagerTransaction) { ref.roundID = "round-2" },
			want:   FailureReferenceMismatch,
		},
		{
			name: "referência de outro jogador", kind: KindRefund, amount: "40.00", prepare: betSetup,
			mutate: func(ref *WagerTransaction) { ref.playerID = uuid.New() },
			want:   FailureReferenceMismatch,
		},
		{
			name: "referência de outra carteira", kind: KindRefund, amount: "40.00", prepare: betSetup,
			mutate: func(ref *WagerTransaction) { ref.walletID = uuid.New() },
			want:   FailureReferenceMismatch,
		},
		{
			name: "referência de outro provedor", kind: KindRefund, amount: "40.00", prepare: betSetup,
			mutate: func(ref *WagerTransaction) { ref.providerID = "provider-b" },
			want:   FailureReferenceMismatch,
		},
		{
			name: "referência rejeitada", kind: KindRefund, amount: "40.00",
			prepare: func(t *testing.T) setup {
				s := newScenario(t, "10.00")
				bet := s.op(KindBet, "40.00", "bet-1", "")
				s.process(bet, nil, false) // sem saldo: REJECTED
				return setup{s, bet}
			},
			want: FailureReferenceNotProcessed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := tt.prepare(t)
			if tt.mutate != nil {
				tt.mutate(st.ref)
			}
			before := st.s.wallet.Snapshot()
			tx := st.s.op(tt.kind, tt.amount, "rev-1", "bet-1")
			r := st.s.process(tx, st.ref, tt.reversed)
			expectRejected(t, r, tx, tt.want)
			if st.s.wallet.Snapshot() != before {
				t.Error("carteira alterada por reversão rejeitada")
			}
		})
	}
}

func TestReferenceNotYetAvailable(t *testing.T) {
	s := newScenario(t, "100.00")
	rb := s.op(KindRollback, "40.00", "rb-1", "bet-1")

	// Chega antes da BET: aguarda.
	r := s.process(rb, nil, false)
	if r.Outcome != OutcomeAwaitingReference || rb.Status() != StatusPendingReference || rb.Attempts() != 1 {
		t.Fatalf("outcome %s status %s attempts %d", r.Outcome, rb.Status(), rb.Attempts())
	}
	if !rb.NextAttemptAt().After(t0) {
		t.Error("próxima tentativa não agendada")
	}
	s.expectBalance("100.00", 1)

	// A BET chega e é processada; a nova tentativa resolve o ROLLBACK.
	bet := s.processed(KindBet, "40.00", "bet-1", "", nil)
	r = s.process(rb, bet, false)
	if r.Outcome != OutcomeProcessed || rb.ReferenceTransactionID() != bet.ID() {
		t.Fatalf("após referência: outcome %s", r.Outcome)
	}
	s.expectBalance("100.00", 3)
}

func TestReferenceStillPendingKeepsWaiting(t *testing.T) {
	s := newScenario(t, "100.00")
	pendingBet := s.op(KindBet, "40.00", "bet-1", "") // ainda PENDING
	refund := s.op(KindRefund, "40.00", "refund-1", "bet-1")

	r := s.process(refund, pendingBet, false)
	if r.Outcome != OutcomeAwaitingReference || refund.ReferenceTransactionID() != uuid.Nil {
		t.Errorf("outcome %s, referência %s", r.Outcome, refund.ReferenceTransactionID())
	}
}

func TestReferenceExpires(t *testing.T) {
	s := newScenario(t, "100.00")
	rb := s.op(KindRollback, "40.00", "rb-1", "bet-1")
	policy := ReferenceRetryPolicy{BaseDelay: time.Second, MaxDelay: time.Second, MaxAttempts: 2}

	var r ProcessResult
	for i := 0; i < 3; i++ {
		var err error
		r, err = ProcessWager(ProcessInput{
			Transaction: rb, Wallet: s.wallet, LedgerEntryID: uuid.New(), RetryPolicy: policy, Now: t0,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if r.Outcome != OutcomeRejected || rb.FailureCode() != FailureReferenceNotFound {
		t.Errorf("outcome %s código %s, want REJECTED REFERENCE_NOT_FOUND", r.Outcome, rb.FailureCode())
	}
}

func TestProcessWalletMismatch(t *testing.T) {
	t.Run("jogador diferente", func(t *testing.T) {
		s := newScenario(t, "100.00")
		tx := s.op(KindBet, "10.00", "bet-1", "")
		tx.playerID = uuid.New()
		expectRejected(t, s.process(tx, nil, false), tx, FailureWalletPlayerMismatch)
		s.expectBalance("100.00", 1)
	})
	t.Run("moeda diferente", func(t *testing.T) {
		s := newScenario(t, "100.00")
		tx := s.op(KindBet, "10.00", "bet-1", "")
		tx.money = mustMinor(t, 1000, usd)
		expectRejected(t, s.process(tx, nil, false), tx, FailureCurrencyMismatch)
		s.expectBalance("100.00", 1)
	})
}

func TestProcessProgrammingErrors(t *testing.T) {
	s := newScenario(t, "100.00")
	processedBet := s.processed(KindBet, "10.00", "bet-1", "", nil)

	other := newTestWallet(t, "100.00")
	tx := s.op(KindBet, "10.00", "bet-2", "")

	tests := []struct {
		name string
		in   ProcessInput
		want error
	}{
		{"transação terminal", ProcessInput{Transaction: processedBet, Wallet: s.wallet, LedgerEntryID: uuid.New(), Now: t0}, ErrInvalidTransition},
		{"carteira errada", ProcessInput{Transaction: tx, Wallet: other, LedgerEntryID: uuid.New(), Now: t0}, ErrInvalidTransaction},
		{"sem carteira", ProcessInput{Transaction: tx, LedgerEntryID: uuid.New(), Now: t0}, ErrUninitialized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ProcessWager(tt.in); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestOpenWallet(t *testing.T) {
	p := OpenWalletParams{
		WalletID:       uuid.New(),
		PlayerID:       uuid.New(),
		OpeningID:      uuid.New(),
		LedgerEntryID:  uuid.New(),
		InitialBalance: mustMoney(t, "1000.00", "BRL"),
		Now:            t0,
	}
	r, err := OpenWallet(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Wallet.Version() != 1 || r.Wallet.Balance().Amount() != "1000.00" {
		t.Errorf("carteira = %s v%d", r.Wallet.Balance(), r.Wallet.Version())
	}

	op := r.Opening
	if op == nil || op.Kind() != KindOpening || op.Status() != StatusProcessed || op.IsExternal() {
		t.Fatalf("OPENING incorreto: %+v", op)
	}
	// Metadados externos não se aplicam ao OPENING.
	if op.ProviderID() != "" || op.ExternalTransactionID() != "" || op.IdempotencyKey() != "" ||
		op.PayloadHash() != "" || op.RoundID() != "" || op.GameID() != "" || op.HasReference() {
		t.Errorf("OPENING com metadados externos: %+v", op.Snapshot())
	}
	if bal, _ := op.ResultBalance(); bal.Amount() != "1000.00" || op.ResultWalletVersion() != 1 {
		t.Errorf("resultado do OPENING = %s v%d", bal, op.ResultWalletVersion())
	}

	e := r.LedgerEntry
	if e == nil || e.Direction() != DirectionCredit || e.TransactionID() != op.ID() ||
		e.BalanceBefore().Amount() != "0.00" || e.BalanceAfter().Amount() != "1000.00" || e.WalletVersion() != 1 {
		t.Errorf("lançamento de abertura incorreto: %+v", e)
	}

	// O OPENING persistido pode ser reidratado.
	if _, err := RehydrateTransaction(op.Snapshot()); err != nil {
		t.Errorf("reidratar OPENING: %v", err)
	}
	// E não pode ser processado como operação externa.
	if _, err := ProcessWager(ProcessInput{Transaction: op, Wallet: r.Wallet, Now: t0}); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("ProcessWager(OPENING) = %v, want ErrInvalidTransaction", err)
	}
}

func TestOpenWalletZeroBalance(t *testing.T) {
	r, err := OpenWallet(OpenWalletParams{
		WalletID: uuid.New(), PlayerID: uuid.New(), OpeningID: uuid.New(), LedgerEntryID: uuid.New(),
		InitialBalance: mustMoney(t, "0.00", "BRL"), Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Opening != nil || r.LedgerEntry != nil {
		t.Error("saldo inicial zero não deve criar OPENING nem lançamento")
	}
	if r.Wallet.Version() != 1 || !r.Wallet.Balance().IsZero() {
		t.Errorf("carteira = %s v%d", r.Wallet.Balance(), r.Wallet.Version())
	}
}

func TestOpenWalletInvalid(t *testing.T) {
	base := OpenWalletParams{
		WalletID: uuid.New(), PlayerID: uuid.New(), OpeningID: uuid.New(), LedgerEntryID: uuid.New(), Now: t0,
	}
	neg := base
	neg.InitialBalance = mustMinor(t, -100, BRL)
	if _, err := OpenWallet(neg); !errors.Is(err, ErrNegativeAmount) {
		t.Errorf("saldo negativo = %v", err)
	}
	if _, err := OpenWallet(base); !errors.Is(err, ErrUninitialized) {
		t.Errorf("saldo não inicializado = %v", err)
	}
}
