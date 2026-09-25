package app

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
)

type OpenWalletCommand struct {
	Principal     Principal
	PlayerID      string
	Amount        string
	Currency      string
	CorrelationID string
}

// OpenWallet abre uma carteira. Com saldo inicial positivo, grava no mesmo
// commit a carteira, o OPENING, o lançamento de crédito e os eventos
// WagerTransactionProcessed e WalletBalanceChanged.
func (s *Service) OpenWallet(ctx context.Context, cmd OpenWalletCommand) (*domain.Wallet, error) {
	if !cmd.Principal.Internal {
		return nil, fmt.Errorf("%w: abertura de carteira é restrita ao serviço interno", ErrForbidden)
	}
	playerID, err := uuid.Parse(cmd.PlayerID)
	if err != nil {
		return nil, fmt.Errorf("%w: playerId: %w", ErrInvalidInput, err)
	}
	initial, err := domain.ParseMoney(cmd.Amount, cmd.Currency)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}

	opened, err := domain.OpenWallet(domain.OpenWalletParams{
		WalletID:       s.newID(),
		PlayerID:       playerID,
		OpeningID:      s.newID(),
		LedgerEntryID:  s.newID(),
		InitialBalance: initial,
		Now:            s.clock.Now(),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}

	meta := domain.EventMeta{CorrelationID: cmd.CorrelationID}
	if meta.CorrelationID == "" {
		meta.CorrelationID = opened.Wallet.ID().String()
	}

	err = s.store.InTx(ctx, func(ctx context.Context, r Repos) error {
		if err := r.Wallets().Insert(ctx, opened.Wallet.Snapshot()); err != nil {
			return err
		}
		if opened.Opening == nil {
			return nil // saldo inicial zero: sem OPENING, ledger nem eventos financeiros
		}
		if err := r.Transactions().Insert(ctx, opened.Opening.Snapshot()); err != nil {
			return err
		}
		if err := r.Ledger().Insert(ctx, *opened.LedgerEntry); err != nil {
			return err
		}
		events, err := domain.TransactionEvents(opened.Opening, opened.LedgerEntry, meta, s.newID)
		if err != nil {
			return err
		}
		return r.Outbox().Insert(ctx, events)
	})
	if err != nil {
		return nil, err
	}
	return opened.Wallet, nil
}
