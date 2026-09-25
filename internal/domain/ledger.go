package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Direction indica se um lançamento debita ou credita a carteira.
type Direction string

const (
	DirectionDebit  Direction = "DEBIT"
	DirectionCredit Direction = "CREDIT"
)

// ParseDirection valida uma direção vinda da persistência.
func ParseDirection(s string) (Direction, error) {
	switch d := Direction(s); d {
	case DirectionDebit, DirectionCredit:
		return d, nil
	default:
		return "", fmt.Errorf("%w: direction %q", ErrInvalidLedgerEntry, s)
	}
}

// LedgerEntry é um lançamento imutável do livro-razão da carteira.
// Não há setters: depois de construído, nenhum campo muda.
type LedgerEntry struct {
	id            uuid.UUID
	walletID      uuid.UUID
	transactionID uuid.UUID
	direction     Direction
	amount        Money
	balanceBefore Money
	balanceAfter  Money
	walletVersion int64
	createdAt     time.Time
}

// LedgerEntryParams reúne os campos de um lançamento.
type LedgerEntryParams struct {
	ID            uuid.UUID
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     Direction
	Amount        Money
	BalanceBefore Money
	BalanceAfter  Money
	WalletVersion int64
	CreatedAt     time.Time
}

// NewLedgerEntry cria um lançamento validando balanceAfter = balanceBefore ± amount
// conforme a direção. Em geral é chamado pela Wallet ao debitar ou creditar.
func NewLedgerEntry(p LedgerEntryParams) (LedgerEntry, error) {
	if err := validateLedgerEntry(p); err != nil {
		return LedgerEntry{}, err
	}
	return LedgerEntry{
		id:            p.ID,
		walletID:      p.WalletID,
		transactionID: p.TransactionID,
		direction:     p.Direction,
		amount:        p.Amount,
		balanceBefore: p.BalanceBefore,
		balanceAfter:  p.BalanceAfter,
		walletVersion: p.WalletVersion,
		createdAt:     p.CreatedAt.UTC(),
	}, nil
}

// RehydrateLedgerEntry reconstrói um lançamento lido do banco. Aplica as mesmas
// validações de NewLedgerEntry, sem efeito colateral algum.
func RehydrateLedgerEntry(p LedgerEntryParams) (LedgerEntry, error) {
	return NewLedgerEntry(p)
}

func validateLedgerEntry(p LedgerEntryParams) error {
	switch {
	case p.ID == uuid.Nil:
		return fmt.Errorf("%w: id vazio", ErrInvalidLedgerEntry)
	case p.WalletID == uuid.Nil:
		return fmt.Errorf("%w: walletId vazio", ErrInvalidLedgerEntry)
	case p.TransactionID == uuid.Nil:
		return fmt.Errorf("%w: transactionId vazio", ErrInvalidLedgerEntry)
	case p.WalletVersion < 1:
		return fmt.Errorf("%w: walletVersion %d", ErrInvalidLedgerEntry, p.WalletVersion)
	case p.CreatedAt.IsZero():
		return fmt.Errorf("%w: createdAt vazio", ErrInvalidLedgerEntry)
	}
	if _, err := ParseDirection(string(p.Direction)); err != nil {
		return err
	}
	if !p.Amount.IsValid() || !p.BalanceBefore.IsValid() || !p.BalanceAfter.IsValid() {
		return fmt.Errorf("%w: valores não inicializados", ErrInvalidLedgerEntry)
	}
	if !p.Amount.IsPositive() {
		return fmt.Errorf("%w: %s", ErrNonPositiveAmount, p.Amount)
	}
	if p.BalanceBefore.IsNegative() || p.BalanceAfter.IsNegative() {
		return fmt.Errorf("%w: saldo negativo", ErrInvalidLedgerEntry)
	}

	var expected Money
	var err error
	if p.Direction == DirectionCredit {
		expected, err = p.BalanceBefore.Add(p.Amount)
	} else {
		expected, err = p.BalanceBefore.Sub(p.Amount)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidLedgerEntry, err)
	}
	if cmp, err := expected.Cmp(p.BalanceAfter); err != nil || cmp != 0 {
		return fmt.Errorf("%w: %s %s %s != %s", ErrInvalidLedgerEntry,
			p.BalanceBefore, p.Direction, p.Amount, p.BalanceAfter)
	}
	return nil
}

func (e LedgerEntry) ID() uuid.UUID            { return e.id }
func (e LedgerEntry) WalletID() uuid.UUID      { return e.walletID }
func (e LedgerEntry) TransactionID() uuid.UUID { return e.transactionID }
func (e LedgerEntry) Direction() Direction     { return e.direction }
func (e LedgerEntry) Amount() Money            { return e.amount }
func (e LedgerEntry) BalanceBefore() Money     { return e.balanceBefore }
func (e LedgerEntry) BalanceAfter() Money      { return e.balanceAfter }
func (e LedgerEntry) WalletVersion() int64     { return e.walletVersion }
func (e LedgerEntry) CreatedAt() time.Time     { return e.createdAt }

// SignedAmount devolve o efeito do lançamento no saldo: positivo para crédito,
// negativo para débito. Usado na reconciliação.
func (e LedgerEntry) SignedAmount() (Money, error) {
	if e.direction == DirectionDebit {
		return e.amount.Neg()
	}
	return e.amount, nil
}
