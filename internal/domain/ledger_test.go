package domain

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func validEntryParams(t *testing.T) LedgerEntryParams {
	t.Helper()
	return LedgerEntryParams{
		ID:            uuid.New(),
		WalletID:      uuid.New(),
		TransactionID: uuid.New(),
		Direction:     DirectionDebit,
		Amount:        mustMoney(t, "80.00", "BRL"),
		BalanceBefore: mustMoney(t, "100.00", "BRL"),
		BalanceAfter:  mustMoney(t, "20.00", "BRL"),
		WalletVersion: 2,
		CreatedAt:     t0,
	}
}

func TestNewLedgerEntry(t *testing.T) {
	p := validEntryParams(t)
	e, err := NewLedgerEntry(p)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := e.SignedAmount()
	if err != nil || signed.Amount() != "-80.00" {
		t.Errorf("SignedAmount(débito) = %s, %v", signed, err)
	}

	p.Direction = DirectionCredit
	p.BalanceBefore = mustMoney(t, "20.00", "BRL")
	p.BalanceAfter = mustMoney(t, "100.00", "BRL")
	e, err = NewLedgerEntry(p)
	if err != nil {
		t.Fatal(err)
	}
	if signed, _ := e.SignedAmount(); signed.Amount() != "80.00" {
		t.Errorf("SignedAmount(crédito) = %s", signed)
	}
}

func TestNewLedgerEntryInvalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*LedgerEntryParams)
		want   error
	}{
		{"aritmética errada no débito", func(p *LedgerEntryParams) { p.BalanceAfter = mustMoney(t, "30.00", "BRL") }, ErrInvalidLedgerEntry},
		{"direção trocada", func(p *LedgerEntryParams) { p.Direction = DirectionCredit }, ErrInvalidLedgerEntry},
		{"direção inválida", func(p *LedgerEntryParams) { p.Direction = "TRANSFER" }, ErrInvalidLedgerEntry},
		{"valor zero", func(p *LedgerEntryParams) {
			p.Amount = mustMoney(t, "0.00", "BRL")
			p.BalanceAfter = p.BalanceBefore
		}, ErrNonPositiveAmount},
		{"saldo depois negativo", func(p *LedgerEntryParams) {
			p.BalanceBefore = mustMoney(t, "10.00", "BRL")
			p.BalanceAfter = mustMinor(t, -7000, BRL)
		}, ErrInvalidLedgerEntry},
		{"moeda diferente", func(p *LedgerEntryParams) { p.Amount = mustMinor(t, 8000, usd) }, ErrCurrencyMismatch},
		{"valor não inicializado", func(p *LedgerEntryParams) { p.Amount = Money{} }, ErrInvalidLedgerEntry},
		{"id vazio", func(p *LedgerEntryParams) { p.ID = uuid.Nil }, ErrInvalidLedgerEntry},
		{"wallet vazia", func(p *LedgerEntryParams) { p.WalletID = uuid.Nil }, ErrInvalidLedgerEntry},
		{"transação vazia", func(p *LedgerEntryParams) { p.TransactionID = uuid.Nil }, ErrInvalidLedgerEntry},
		{"versão zero", func(p *LedgerEntryParams) { p.WalletVersion = 0 }, ErrInvalidLedgerEntry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validEntryParams(t)
			tt.mutate(&p)
			if _, err := NewLedgerEntry(p); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if _, err := RehydrateLedgerEntry(p); !errors.Is(err, tt.want) {
				t.Errorf("Rehydrate err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestParseDirection(t *testing.T) {
	for _, s := range []string{"DEBIT", "CREDIT"} {
		if _, err := ParseDirection(s); err != nil {
			t.Errorf("ParseDirection(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "debit", "X"} {
		if _, err := ParseDirection(s); !errors.Is(err, ErrInvalidLedgerEntry) {
			t.Errorf("ParseDirection(%q) = %v, want ErrInvalidLedgerEntry", s, err)
		}
	}
}
