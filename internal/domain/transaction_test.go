package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var testHash = strings.Repeat("a", 64)

func externalParams(t *testing.T, kind Kind, amount string) ExternalTransactionParams {
	t.Helper()
	p := ExternalTransactionParams{
		ID:                    uuid.New(),
		ProviderID:            "provider-a",
		ExternalTransactionID: "tx-" + uuid.NewString(),
		PayloadHash:           testHash,
		PlayerID:              uuid.New(),
		WalletID:              uuid.New(),
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  kind,
		Money:                 mustMoney(t, amount, "BRL"),
		Now:                   t0,
	}
	p.IdempotencyKey = p.ProviderID + ":" + p.ExternalTransactionID
	if kind.IsReversal() {
		p.ReferenceExternalTransactionID = "ref-1"
	}
	return p
}

func TestParseExternalKind(t *testing.T) {
	for _, k := range []string{"BET", "WIN", "LOSS", "REFUND", "ROLLBACK"} {
		if _, err := ParseExternalKind(k); err != nil {
			t.Errorf("ParseExternalKind(%s) = %v", k, err)
		}
	}
	if _, err := ParseExternalKind("OPENING"); !errors.Is(err, ErrOpeningNotAllowed) {
		t.Errorf("OPENING externo = %v, want ErrOpeningNotAllowed", err)
	}
	for _, k := range []string{"", "bet", "DEPOSIT"} {
		if _, err := ParseExternalKind(k); !errors.Is(err, ErrInvalidKind) {
			t.Errorf("ParseExternalKind(%q) = %v, want ErrInvalidKind", k, err)
		}
	}
}

func TestNewExternalTransaction(t *testing.T) {
	p := externalParams(t, KindBet, "25.00")
	tx, err := NewExternalTransaction(p)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status() != StatusPending {
		t.Errorf("status inicial = %s, want PENDING", tx.Status())
	}
	if tx.ProviderID() != "provider-a" || tx.Kind() != KindBet || tx.Money().Amount() != "25.00" ||
		tx.IdempotencyKey() != p.IdempotencyKey || tx.PayloadHash() != testHash {
		t.Errorf("campos incorretos: %+v", tx.Snapshot())
	}
	if _, ok := tx.ResultBalance(); ok {
		t.Error("transação nova não deveria ter resultado")
	}
}

// Política de valor zero de cada tipo.
func TestAmountPolicyPerKind(t *testing.T) {
	tests := []struct {
		kind   Kind
		amount string
		want   error
	}{
		{KindBet, "0.00", ErrNonPositiveAmount},
		{KindWin, "0.00", ErrNonPositiveAmount},
		{KindRefund, "0.00", ErrNonPositiveAmount},
		{KindRollback, "0.00", ErrNonPositiveAmount},
		{KindLoss, "0.00", nil},
		{KindLoss, "0.01", ErrInvalidAmount},
		{KindLoss, "25.00", ErrInvalidAmount},
		{KindBet, "0.01", nil},
		{KindWin, "10.00", nil},
		{KindRefund, "10.00", nil},
		{KindRollback, "10.00", nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind)+"_"+tt.amount, func(t *testing.T) {
			_, err := NewExternalTransaction(externalParams(t, tt.kind, tt.amount))
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestReferencePolicyPerKind(t *testing.T) {
	tests := []struct {
		name   string
		kind   Kind
		amount string
		ref    string
		ok     bool
	}{
		{"REFUND sem referência", KindRefund, "1.00", "", false},
		{"ROLLBACK sem referência", KindRollback, "1.00", "", false},
		{"BET com referência", KindBet, "1.00", "x", false},
		{"LOSS com referência", KindLoss, "0.00", "x", false},
		{"WIN sem referência", KindWin, "1.00", "", true},
		{"WIN com referência", KindWin, "1.00", "bet-1", true},
		{"REFUND com referência", KindRefund, "1.00", "bet-1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := externalParams(t, tt.kind, tt.amount)
			p.ReferenceExternalTransactionID = tt.ref
			_, err := NewExternalTransaction(p)
			if tt.ok && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidTransaction) {
				t.Errorf("err = %v, want ErrInvalidTransaction", err)
			}
		})
	}
}

func TestNewExternalTransactionInvalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ExternalTransactionParams)
		want   error
	}{
		{"OPENING", func(p *ExternalTransactionParams) { p.Kind = KindOpening }, ErrOpeningNotAllowed},
		{"tipo desconhecido", func(p *ExternalTransactionParams) { p.Kind = "DEPOSIT" }, ErrInvalidKind},
		{"sem provider", func(p *ExternalTransactionParams) { p.ProviderID = "" }, ErrInvalidTransaction},
		{"sem externalId", func(p *ExternalTransactionParams) { p.ExternalTransactionID = "" }, ErrInvalidTransaction},
		{"sem chave", func(p *ExternalTransactionParams) { p.IdempotencyKey = "" }, ErrInvalidTransaction},
		{"sem rodada", func(p *ExternalTransactionParams) { p.RoundID = "" }, ErrInvalidTransaction},
		{"sem jogo", func(p *ExternalTransactionParams) { p.GameID = "" }, ErrInvalidTransaction},
		{"id externo longo", func(p *ExternalTransactionParams) { p.ExternalTransactionID = strings.Repeat("x", 256) }, ErrInvalidTransaction},
		{"hash inválido", func(p *ExternalTransactionParams) { p.PayloadHash = "abc" }, ErrInvalidTransaction},
		{"hash maiúsculo", func(p *ExternalTransactionParams) { p.PayloadHash = strings.Repeat("A", 64) }, ErrInvalidTransaction},
		{"sem id", func(p *ExternalTransactionParams) { p.ID = uuid.Nil }, ErrInvalidTransaction},
		{"sem carteira", func(p *ExternalTransactionParams) { p.WalletID = uuid.Nil }, ErrInvalidTransaction},
		{"sem jogador", func(p *ExternalTransactionParams) { p.PlayerID = uuid.Nil }, ErrInvalidTransaction},
		{"money não inicializado", func(p *ExternalTransactionParams) { p.Money = Money{} }, ErrUninitialized},
		{"money negativo", func(p *ExternalTransactionParams) { p.Money = mustMinor(t, -100, BRL) }, ErrNegativeAmount},
		{"sem instante", func(p *ExternalTransactionParams) { p.Now = time.Time{} }, ErrInvalidTransaction},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := externalParams(t, KindBet, "10.00")
			tt.mutate(&p)
			if _, err := NewExternalTransaction(p); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestTerminalStatesAreFinal(t *testing.T) {
	newTx := func() *WagerTransaction {
		tx, err := NewExternalTransaction(externalParams(t, KindRefund, "10.00"))
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}
	balance := mustMoney(t, "10.00", "BRL")
	toTerminal := map[Status]func(*WagerTransaction) error{
		StatusProcessed: func(tx *WagerTransaction) error { return tx.markProcessed(balance, 2, t0) },
		StatusRejected: func(tx *WagerTransaction) error {
			return tx.markRejected(FailureInsufficientFunds, balance, 1, t0)
		},
		StatusFailed: func(tx *WagerTransaction) error { return tx.MarkFailed(FailureProcessingFailed, t0) },
	}
	for status, apply := range toTerminal {
		t.Run(string(status), func(t *testing.T) {
			tx := newTx()
			if err := apply(tx); err != nil {
				t.Fatal(err)
			}
			if tx.Status() != status {
				t.Fatalf("status = %s, want %s", tx.Status(), status)
			}
			before := tx.Snapshot()

			for name, again := range toTerminal {
				if err := again(tx); !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("%s -> %s = %v, want ErrInvalidTransition", status, name, err)
				}
			}
			if _, err := tx.awaitReference(DefaultReferenceRetryPolicy, t0); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s -> PENDING_REFERENCE = %v, want ErrInvalidTransition", status, err)
			}
			if tx.Snapshot() != before {
				t.Error("transação terminal foi alterada")
			}
		})
	}
}

func TestFailureCodesByStatus(t *testing.T) {
	tx, _ := NewExternalTransaction(externalParams(t, KindBet, "10.00"))
	if err := tx.markRejected(FailureProcessingFailed, Money{}, 0, t0); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("REJECTED com código de falha = %v", err)
	}
	if err := tx.MarkFailed(FailureInsufficientFunds, t0); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("FAILED com código de rejeição = %v", err)
	}
	if tx.Status() != StatusPending {
		t.Error("status alterado após transição inválida")
	}

	if !FailureInsufficientFunds.IsRejection() || FailureInsufficientFunds.IsCorrectable() {
		t.Error("INSUFFICIENT_FUNDS deveria ser rejeição definitiva")
	}
	if !FailureCurrencyMismatch.IsCorrectable() {
		t.Error("CURRENCY_MISMATCH deveria ser corrigível")
	}
	if !FailureProcessingFailed.IsFailure() || FailureProcessingFailed.IsRejection() {
		t.Error("PROCESSING_FAILED deveria ser falha")
	}
	if FailureInsufficientFunds == FailureInsufficientFundsForReversal {
		t.Error("códigos de saldo insuficiente devem ser distintos")
	}
}

func TestAwaitReferenceBackoffAndExpiry(t *testing.T) {
	policy := ReferenceRetryPolicy{BaseDelay: time.Second, MaxDelay: 4 * time.Second, MaxAttempts: 4}
	tx, err := NewExternalTransaction(externalParams(t, KindRollback, "10.00"))
	if err != nil {
		t.Fatal(err)
	}

	wantDelays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	now := t0
	for i, want := range wantDelays {
		waiting, err := tx.awaitReference(policy, now)
		if err != nil || !waiting {
			t.Fatalf("tentativa %d: waiting=%v err=%v", i, waiting, err)
		}
		if tx.Status() != StatusPendingReference || tx.Attempts() != i+1 {
			t.Errorf("tentativa %d: status %s attempts %d", i, tx.Status(), tx.Attempts())
		}
		if got := tx.NextAttemptAt().Sub(now); got != want {
			t.Errorf("tentativa %d: atraso %v, want %v", i, got, want)
		}
		now = tx.NextAttemptAt()
	}

	waiting, err := tx.awaitReference(policy, now)
	if err != nil || waiting {
		t.Fatalf("após esgotar: waiting=%v err=%v", waiting, err)
	}
	if tx.Status() != StatusRejected || tx.FailureCode() != FailureReferenceNotFound {
		t.Errorf("esgotado: status %s código %s", tx.Status(), tx.FailureCode())
	}
}

func TestAwaitReferenceRequiresReference(t *testing.T) {
	tx, _ := NewExternalTransaction(externalParams(t, KindBet, "10.00"))
	if _, err := tx.awaitReference(DefaultReferenceRetryPolicy, t0); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("BET aguardando referência = %v, want ErrInvalidTransition", err)
	}
}

func TestRetryPolicyDelay(t *testing.T) {
	p := DefaultReferenceRetryPolicy
	if p.Delay(0) != time.Second || p.Delay(3) != 8*time.Second {
		t.Errorf("Delay(0)=%v Delay(3)=%v", p.Delay(0), p.Delay(3))
	}
	for _, attempt := range []int{9, 30, 62, 63, 1000} {
		if d := p.Delay(attempt); d != p.MaxDelay {
			t.Errorf("Delay(%d) = %v, want teto %v", attempt, d, p.MaxDelay)
		}
	}
}

func TestRehydrateTransaction(t *testing.T) {
	tx, _ := NewExternalTransaction(externalParams(t, KindBet, "25.00"))
	if err := tx.markProcessed(mustMoney(t, "975.00", "BRL"), 2, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	snap := tx.Snapshot()

	back, err := RehydrateTransaction(snap)
	if err != nil {
		t.Fatal(err)
	}
	if back.Snapshot() != snap {
		t.Errorf("reidratação alterou o estado:\n%+v\n%+v", back.Snapshot(), snap)
	}
	if bal, ok := back.ResultBalance(); !ok || bal.Amount() != "975.00" {
		t.Errorf("saldo do resultado = %s", bal)
	}

	invalid := []struct {
		name   string
		mutate func(*TransactionSnapshot)
	}{
		{"PROCESSED sem resultado", func(s *TransactionSnapshot) { s.ResultBalance = Money{} }},
		{"REJECTED sem código", func(s *TransactionSnapshot) { s.Status = StatusRejected; s.FailureCode = "" }},
		{"status desconhecido", func(s *TransactionSnapshot) { s.Status = "DONE" }},
		{"OPENING com provider", func(s *TransactionSnapshot) { s.Kind = KindOpening }},
		{"PENDING_REFERENCE em BET", func(s *TransactionSnapshot) { s.Status = StatusPendingReference }},
		{"LOSS com valor", func(s *TransactionSnapshot) { s.Kind = KindLoss }},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			s := snap
			tt.mutate(&s)
			if _, err := RehydrateTransaction(s); err == nil {
				t.Error("esperava erro")
			}
		})
	}
}
