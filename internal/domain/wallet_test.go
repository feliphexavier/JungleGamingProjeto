package domain

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

var t0 = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func newTestWallet(t *testing.T, balance string) *Wallet {
	t.Helper()
	w, err := NewWallet(uuid.New(), uuid.New(), mustMoney(t, balance, "BRL"), t0)
	if err != nil {
		t.Fatalf("NewWallet: %v", err)
	}
	return w
}

func TestNewWallet(t *testing.T) {
	id, player := uuid.New(), uuid.New()
	w, err := NewWallet(id, player, mustMoney(t, "1000.00", "BRL"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if w.ID() != id || w.PlayerID() != player {
		t.Error("identidade incorreta")
	}
	if w.Balance().Amount() != "1000.00" || w.Currency() != BRL {
		t.Errorf("saldo = %s", w.Balance())
	}
	if w.Version() != 1 {
		t.Errorf("versão inicial = %d, want 1", w.Version())
	}
	if !w.CreatedAt().Equal(t0) || !w.UpdatedAt().Equal(t0) {
		t.Error("timestamps incorretos")
	}

	zero, err := NewWallet(uuid.New(), uuid.New(), mustMoney(t, "0.00", "BRL"), t0)
	if err != nil || !zero.Balance().IsZero() || zero.Version() != 1 {
		t.Errorf("carteira com saldo zero: %v, %v", zero, err)
	}
}

func TestNewWalletInvalid(t *testing.T) {
	brl := mustMoney(t, "10.00", "BRL")
	tests := []struct {
		name    string
		id      uuid.UUID
		player  uuid.UUID
		balance Money
		now     time.Time
	}{
		{"id vazio", uuid.Nil, uuid.New(), brl, t0},
		{"player vazio", uuid.New(), uuid.Nil, brl, t0},
		{"saldo não inicializado", uuid.New(), uuid.New(), Money{}, t0},
		{"saldo negativo", uuid.New(), uuid.New(), mustMinor(t, -1, BRL), t0},
		{"sem timestamp", uuid.New(), uuid.New(), brl, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewWallet(tt.id, tt.player, tt.balance, tt.now); !errors.Is(err, ErrInvalidWallet) {
				t.Errorf("err = %v, want ErrInvalidWallet", err)
			}
		})
	}
}

func TestRehydrateWallet(t *testing.T) {
	snap := WalletSnapshot{
		ID:        uuid.New(),
		PlayerID:  uuid.New(),
		Balance:   mustMoney(t, "975.00", "BRL"),
		Version:   7,
		CreatedAt: t0,
		UpdatedAt: t0.Add(time.Hour),
	}
	w, err := RehydrateWallet(snap)
	if err != nil {
		t.Fatal(err)
	}
	// Reidratar não reaplica nada: estado idêntico ao persistido.
	if w.Snapshot() != snap {
		t.Errorf("Snapshot() = %+v, want %+v", w.Snapshot(), snap)
	}

	invalid := []struct {
		name   string
		mutate func(*WalletSnapshot)
	}{
		{"versão zero", func(s *WalletSnapshot) { s.Version = 0 }},
		{"saldo negativo", func(s *WalletSnapshot) { s.Balance = mustMinor(t, -100, BRL) }},
		{"updatedAt antes de createdAt", func(s *WalletSnapshot) { s.UpdatedAt = t0.Add(-time.Second) }},
		{"id vazio", func(s *WalletSnapshot) { s.ID = uuid.Nil }},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			s := snap
			tt.mutate(&s)
			if _, err := RehydrateWallet(s); !errors.Is(err, ErrInvalidWallet) {
				t.Errorf("err = %v, want ErrInvalidWallet", err)
			}
		})
	}
}

func TestDebit(t *testing.T) {
	w := newTestWallet(t, "100.00")
	entryID, txID := uuid.New(), uuid.New()
	now := t0.Add(time.Minute)

	entry, err := w.Debit(entryID, txID, mustMoney(t, "80.00", "BRL"), now)
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "20.00" || w.Version() != 2 || !w.UpdatedAt().Equal(now) {
		t.Errorf("carteira após débito: saldo %s versão %d", w.Balance(), w.Version())
	}
	if entry.ID() != entryID || entry.TransactionID() != txID || entry.WalletID() != w.ID() {
		t.Error("identidade do lançamento incorreta")
	}
	if entry.Direction() != DirectionDebit || entry.Amount().Amount() != "80.00" ||
		entry.BalanceBefore().Amount() != "100.00" || entry.BalanceAfter().Amount() != "20.00" ||
		entry.WalletVersion() != 2 {
		t.Errorf("lançamento incorreto: %+v", entry)
	}
}

func TestDebitInsufficientFunds(t *testing.T) {
	// Cenário do desafio: 100.00, duas apostas de 80.00.
	w := newTestWallet(t, "100.00")
	if _, err := w.Debit(uuid.New(), uuid.New(), mustMoney(t, "80.00", "BRL"), t0); err != nil {
		t.Fatal(err)
	}
	before := w.Snapshot()

	_, err := w.Debit(uuid.New(), uuid.New(), mustMoney(t, "80.00", "BRL"), t0)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	if w.Snapshot() != before {
		t.Error("carteira alterada após débito recusado")
	}
	if w.Balance().Amount() != "20.00" {
		t.Errorf("saldo final = %s, want 20.00", w.Balance())
	}
}

func TestDebitExactBalance(t *testing.T) {
	w := newTestWallet(t, "50.00")
	if _, err := w.Debit(uuid.New(), uuid.New(), mustMoney(t, "50.00", "BRL"), t0); err != nil {
		t.Fatal(err)
	}
	if !w.Balance().IsZero() {
		t.Errorf("saldo = %s, want 0.00", w.Balance())
	}
}

func TestCredit(t *testing.T) {
	w := newTestWallet(t, "0.00")
	entry, err := w.Credit(uuid.New(), uuid.New(), mustMoney(t, "25.00", "BRL"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "25.00" || w.Version() != 2 {
		t.Errorf("saldo %s versão %d", w.Balance(), w.Version())
	}
	if entry.Direction() != DirectionCredit || entry.BalanceBefore().Amount() != "0.00" ||
		entry.BalanceAfter().Amount() != "25.00" {
		t.Errorf("lançamento incorreto: %+v", entry)
	}
}

func TestMovementRejections(t *testing.T) {
	tests := []struct {
		name   string
		amount Money
		want   error
	}{
		{"valor zero", mustMoney(t, "0.00", "BRL"), ErrNonPositiveAmount},
		{"valor negativo", mustMinor(t, -100, BRL), ErrNonPositiveAmount},
		{"outra moeda", mustMinor(t, 100, usd), ErrCurrencyMismatch},
		{"não inicializado", Money{}, ErrUninitialized},
	}
	for _, tt := range tests {
		for _, op := range []string{"debit", "credit"} {
			t.Run(tt.name+"/"+op, func(t *testing.T) {
				w := newTestWallet(t, "100.00")
				before := w.Snapshot()
				var err error
				if op == "debit" {
					_, err = w.Debit(uuid.New(), uuid.New(), tt.amount, t0)
				} else {
					_, err = w.Credit(uuid.New(), uuid.New(), tt.amount, t0)
				}
				if !errors.Is(err, tt.want) {
					t.Errorf("err = %v, want %v", err, tt.want)
				}
				if w.Snapshot() != before {
					t.Error("carteira alterada após movimentação recusada")
				}
			})
		}
	}
}

func TestCreditOverflow(t *testing.T) {
	w, err := RehydrateWallet(WalletSnapshot{
		ID: uuid.New(), PlayerID: uuid.New(),
		Balance: mustMinor(t, math.MaxInt64, BRL), Version: 1, CreatedAt: t0, UpdatedAt: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Credit(uuid.New(), uuid.New(), mustMinor(t, 1, BRL), t0); !errors.Is(err, ErrAmountOverflow) {
		t.Errorf("err = %v, want ErrAmountOverflow", err)
	}
	if w.Version() != 1 {
		t.Error("versão alterada após overflow")
	}
}

func TestMovementRequiresValidIDs(t *testing.T) {
	w := newTestWallet(t, "100.00")
	if _, err := w.Debit(uuid.Nil, uuid.New(), mustMoney(t, "1.00", "BRL"), t0); !errors.Is(err, ErrInvalidLedgerEntry) {
		t.Errorf("entryID vazio = %v, want ErrInvalidLedgerEntry", err)
	}
	if _, err := w.Credit(uuid.New(), uuid.Nil, mustMoney(t, "1.00", "BRL"), t0); !errors.Is(err, ErrInvalidLedgerEntry) {
		t.Errorf("transactionID vazio = %v, want ErrInvalidLedgerEntry", err)
	}
	if w.Version() != 1 || w.Balance().Amount() != "100.00" {
		t.Error("carteira alterada após lançamento inválido")
	}
}

func TestUpdatedAtNeverGoesBack(t *testing.T) {
	w := newTestWallet(t, "100.00")
	if _, err := w.Credit(uuid.New(), uuid.New(), mustMoney(t, "1.00", "BRL"), t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if w.UpdatedAt().Before(w.CreatedAt()) {
		t.Errorf("updatedAt %v antes de createdAt %v", w.UpdatedAt(), w.CreatedAt())
	}
}

func TestVersionIncrementsOnlyOnBalanceChange(t *testing.T) {
	w := newTestWallet(t, "100.00")
	ops := []func() error{
		func() error { _, err := w.Debit(uuid.New(), uuid.New(), mustMoney(t, "10.00", "BRL"), t0); return err },
		func() error { _, err := w.Debit(uuid.New(), uuid.New(), mustMoney(t, "999.00", "BRL"), t0); return err }, // recusado
		func() error { _, err := w.Credit(uuid.New(), uuid.New(), mustMoney(t, "5.00", "BRL"), t0); return err },
		func() error { _, err := w.CanDebit(mustMoney(t, "1.00", "BRL")); return err }, // consulta
	}
	for _, op := range ops {
		_ = op()
	}
	if w.Version() != 3 {
		t.Errorf("versão = %d, want 3 (duas mudanças de saldo)", w.Version())
	}
	if w.Balance().Amount() != "95.00" {
		t.Errorf("saldo = %s, want 95.00", w.Balance())
	}
}

func TestCanDebit(t *testing.T) {
	w := newTestWallet(t, "100.00")
	for amount, want := range map[string]bool{"99.99": true, "100.00": true, "100.01": false} {
		got, err := w.CanDebit(mustMoney(t, amount, "BRL"))
		if err != nil || got != want {
			t.Errorf("CanDebit(%s) = %v, %v; want %v", amount, got, err, want)
		}
	}
	if w.Version() != 1 {
		t.Error("CanDebit alterou a carteira")
	}
}

func TestOpeningEntry(t *testing.T) {
	w := newTestWallet(t, "1000.00")
	txID := uuid.New()
	entry, err := w.OpeningEntry(uuid.New(), txID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Direction() != DirectionCredit || entry.Amount().Amount() != "1000.00" ||
		entry.BalanceBefore().Amount() != "0.00" || entry.BalanceAfter().Amount() != "1000.00" ||
		entry.WalletVersion() != 1 || entry.TransactionID() != txID {
		t.Errorf("lançamento de abertura incorreto: %+v", entry)
	}
	if w.Version() != 1 || w.Balance().Amount() != "1000.00" {
		t.Error("OpeningEntry alterou a carteira")
	}

	// Saldo inicial zero não gera lançamento.
	if _, err := newTestWallet(t, "0.00").OpeningEntry(uuid.New(), uuid.New()); !errors.Is(err, ErrNonPositiveAmount) {
		t.Errorf("abertura com zero = %v, want ErrNonPositiveAmount", err)
	}

	// Depois de movimentada, a carteira não aceita mais lançamento de abertura.
	if _, err := w.Credit(uuid.New(), uuid.New(), mustMoney(t, "1.00", "BRL"), t0); err != nil {
		t.Fatal(err)
	}
	if _, err := w.OpeningEntry(uuid.New(), uuid.New()); !errors.Is(err, ErrInvalidWallet) {
		t.Errorf("abertura após movimentação = %v, want ErrInvalidWallet", err)
	}
}

func TestUninitializedWallet(t *testing.T) {
	var w Wallet
	if _, err := w.Debit(uuid.New(), uuid.New(), mustMoney(t, "1.00", "BRL"), t0); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Wallet{}.Debit = %v, want ErrUninitialized", err)
	}
	var nilWallet *Wallet
	if _, err := nilWallet.Credit(uuid.New(), uuid.New(), mustMoney(t, "1.00", "BRL"), t0); !errors.Is(err, ErrUninitialized) {
		t.Errorf("(*Wallet)(nil).Credit = %v, want ErrUninitialized", err)
	}
}
