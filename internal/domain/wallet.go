package domain

import (
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
)

// InitialWalletVersion é a versão de toda carteira recém-criada.
const InitialWalletVersion int64 = 1

// Wallet é a raiz do agregado financeiro. O saldo só muda por Debit e Credit,
// que também produzem o lançamento correspondente do ledger; persistir os dois
// na mesma transação SQL é responsabilidade do caso de uso.
type Wallet struct {
	id        uuid.UUID
	playerID  uuid.UUID
	balance   Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// NewWallet abre uma carteira com o saldo inicial informado (zero ou positivo)
// e versão 1. O crédito inicial (OPENING) e seu lançamento são criados pelo
// fluxo de abertura, não aqui.
func NewWallet(id, playerID uuid.UUID, initialBalance Money, now time.Time) (*Wallet, error) {
	w := &Wallet{
		id:        id,
		playerID:  playerID,
		balance:   initialBalance,
		version:   InitialWalletVersion,
		createdAt: now.UTC(),
		updatedAt: now.UTC(),
	}
	if err := w.validate(); err != nil {
		return nil, err
	}
	return w, nil
}

// WalletSnapshot contém o estado persistido de uma carteira.
type WalletSnapshot struct {
	ID        uuid.UUID
	PlayerID  uuid.UUID
	Balance   Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RehydrateWallet reconstrói uma carteira lida do banco. Apenas valida o
// estado; não reaplica movimentações nem gera lançamentos ou eventos.
func RehydrateWallet(s WalletSnapshot) (*Wallet, error) {
	w := &Wallet{
		id:        s.ID,
		playerID:  s.PlayerID,
		balance:   s.Balance,
		version:   s.Version,
		createdAt: s.CreatedAt.UTC(),
		updatedAt: s.UpdatedAt.UTC(),
	}
	if err := w.validate(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Wallet) validate() error {
	switch {
	case w.id == uuid.Nil:
		return fmt.Errorf("%w: id vazio", ErrInvalidWallet)
	case w.playerID == uuid.Nil:
		return fmt.Errorf("%w: playerId vazio", ErrInvalidWallet)
	case !w.balance.IsValid():
		return fmt.Errorf("%w: saldo não inicializado", ErrInvalidWallet)
	case w.balance.IsNegative():
		return fmt.Errorf("%w: saldo negativo %s", ErrInvalidWallet, w.balance)
	case w.version < InitialWalletVersion:
		return fmt.Errorf("%w: versão %d", ErrInvalidWallet, w.version)
	case w.createdAt.IsZero() || w.updatedAt.IsZero():
		return fmt.Errorf("%w: timestamps vazios", ErrInvalidWallet)
	case w.updatedAt.Before(w.createdAt):
		return fmt.Errorf("%w: updatedAt anterior a createdAt", ErrInvalidWallet)
	}
	return nil
}

func (w *Wallet) ID() uuid.UUID        { return w.id }
func (w *Wallet) PlayerID() uuid.UUID  { return w.playerID }
func (w *Wallet) Currency() Currency   { return w.balance.Currency() }
func (w *Wallet) Balance() Money       { return w.balance }
func (w *Wallet) Version() int64       { return w.version }
func (w *Wallet) CreatedAt() time.Time { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time { return w.updatedAt }

// Snapshot devolve o estado atual para persistência.
func (w *Wallet) Snapshot() WalletSnapshot {
	return WalletSnapshot{
		ID:        w.id,
		PlayerID:  w.playerID,
		Balance:   w.balance,
		Version:   w.version,
		CreatedAt: w.createdAt,
		UpdatedAt: w.updatedAt,
	}
}

// CanDebit informa se o saldo cobre o valor, sem alterar a carteira.
func (w *Wallet) CanDebit(amount Money) (bool, error) {
	if err := w.checkMovement(amount); err != nil {
		return false, err
	}
	cmp, err := w.balance.Cmp(amount)
	if err != nil {
		return false, err
	}
	return cmp >= 0, nil
}

// Debit subtrai o valor do saldo, incrementa a versão e devolve o lançamento
// de débito. Retorna ErrInsufficientFunds sem alterar nada se o saldo não cobrir.
func (w *Wallet) Debit(entryID, transactionID uuid.UUID, amount Money, now time.Time) (LedgerEntry, error) {
	return w.move(DirectionDebit, entryID, transactionID, amount, now)
}

// Credit soma o valor ao saldo, incrementa a versão e devolve o lançamento de crédito.
func (w *Wallet) Credit(entryID, transactionID uuid.UUID, amount Money, now time.Time) (LedgerEntry, error) {
	return w.move(DirectionCredit, entryID, transactionID, amount, now)
}

// OpeningEntry devolve o lançamento de crédito do saldo inicial de uma carteira
// recém-criada (0 -> saldo inicial, versão 1). Só é válido na versão inicial e
// com saldo positivo; não altera a carteira.
func (w *Wallet) OpeningEntry(entryID, transactionID uuid.UUID) (LedgerEntry, error) {
	if w.version != InitialWalletVersion {
		return LedgerEntry{}, fmt.Errorf("%w: abertura só na versão inicial", ErrInvalidWallet)
	}
	if !w.balance.IsPositive() {
		return LedgerEntry{}, fmt.Errorf("%w: saldo inicial %s", ErrNonPositiveAmount, w.balance)
	}
	zero, err := Zero(w.Currency())
	if err != nil {
		return LedgerEntry{}, err
	}
	return NewLedgerEntry(LedgerEntryParams{
		ID:            entryID,
		WalletID:      w.id,
		TransactionID: transactionID,
		Direction:     DirectionCredit,
		Amount:        w.balance,
		BalanceBefore: zero,
		BalanceAfter:  w.balance,
		WalletVersion: w.version,
		CreatedAt:     w.createdAt,
	})
}

func (w *Wallet) move(dir Direction, entryID, transactionID uuid.UUID, amount Money, now time.Time) (LedgerEntry, error) {
	if err := w.checkMovement(amount); err != nil {
		return LedgerEntry{}, err
	}
	if w.version == math.MaxInt64 {
		return LedgerEntry{}, ErrVersionOverflow
	}

	var after Money
	var err error
	if dir == DirectionDebit {
		after, err = w.balance.Sub(amount)
		if err == nil && after.IsNegative() {
			return LedgerEntry{}, fmt.Errorf("%w: saldo %s, débito %s", ErrInsufficientFunds, w.balance, amount)
		}
	} else {
		after, err = w.balance.Add(amount)
	}
	if err != nil {
		return LedgerEntry{}, err
	}

	now = now.UTC()
	if now.Before(w.updatedAt) {
		// Relógios de instâncias diferentes podem divergir; o updatedAt nunca retrocede.
		now = w.updatedAt
	}

	entry, err := NewLedgerEntry(LedgerEntryParams{
		ID:            entryID,
		WalletID:      w.id,
		TransactionID: transactionID,
		Direction:     dir,
		Amount:        amount,
		BalanceBefore: w.balance,
		BalanceAfter:  after,
		WalletVersion: w.version + 1,
		CreatedAt:     now,
	})
	if err != nil {
		return LedgerEntry{}, err
	}

	// Só altera o estado depois que tudo foi validado.
	w.balance = after
	w.version++
	w.updatedAt = now
	return entry, nil
}

func (w *Wallet) checkMovement(amount Money) error {
	if w == nil || w.id == uuid.Nil {
		return fmt.Errorf("%w: wallet", ErrUninitialized)
	}
	if !amount.IsValid() {
		return fmt.Errorf("%w: amount", ErrUninitialized)
	}
	if amount.Currency() != w.Currency() {
		return fmt.Errorf("%w: carteira %s, movimentação %s", ErrCurrencyMismatch, w.Currency(), amount.Currency())
	}
	if !amount.IsPositive() {
		return fmt.Errorf("%w: %s", ErrNonPositiveAmount, amount)
	}
	return nil
}
