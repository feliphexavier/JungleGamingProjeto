package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
)

func requireInternal(p Principal) error {
	if !p.Internal {
		return fmt.Errorf("%w: operação restrita ao serviço interno", ErrForbidden)
	}
	return nil
}

// GetWallet devolve a carteira. Restrito ao serviço interno.
func (s *Service) GetWallet(ctx context.Context, p Principal, walletID uuid.UUID) (*domain.Wallet, error) {
	if err := requireInternal(p); err != nil {
		return nil, err
	}
	return s.store.Reads().Wallets().Get(ctx, walletID)
}

const (
	DefaultLedgerPageSize = 50
	MaxLedgerPageSize     = 200
	ledgerCursorPrefix    = "v1:"
)

// LedgerPage é uma página do ledger. NextCursor vazio indica o fim.
type LedgerPage struct {
	Entries    []domain.LedgerEntry
	NextCursor string
}

// ListLedger pagina o ledger em ordem crescente de versão da carteira. O
// cursor é opaco para o cliente (base64 da última versão lida).
func (s *Service) ListLedger(ctx context.Context, p Principal, walletID uuid.UUID, cursor string, limit int) (LedgerPage, error) {
	if err := requireInternal(p); err != nil {
		return LedgerPage{}, err
	}
	if limit <= 0 {
		limit = DefaultLedgerPageSize
	}
	if limit > MaxLedgerPageSize {
		return LedgerPage{}, fmt.Errorf("%w: limit máximo é %d", ErrInvalidInput, MaxLedgerPageSize)
	}
	after, err := decodeLedgerCursor(cursor)
	if err != nil {
		return LedgerPage{}, err
	}

	reads := s.store.Reads()
	if _, err := reads.Wallets().Get(ctx, walletID); err != nil {
		return LedgerPage{}, err
	}
	// Busca um item a mais para saber se existe próxima página.
	entries, err := reads.Ledger().List(ctx, walletID, after, limit+1)
	if err != nil {
		return LedgerPage{}, err
	}
	page := LedgerPage{Entries: entries}
	if len(entries) > limit {
		page.Entries = entries[:limit]
		page.NextCursor = encodeLedgerCursor(page.Entries[limit-1].WalletVersion())
	}
	return page, nil
}

func encodeLedgerCursor(version int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(ledgerCursorPrefix + strconv.FormatInt(version, 10)))
}

func decodeLedgerCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || !strings.HasPrefix(string(raw), ledgerCursorPrefix) {
		return 0, fmt.Errorf("%w: cursor inválido", ErrInvalidInput)
	}
	v, err := strconv.ParseInt(strings.TrimPrefix(string(raw), ledgerCursorPrefix), 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%w: cursor inválido", ErrInvalidInput)
	}
	return v, nil
}

// GetTransaction devolve uma transação pelo id interno. Um provedor só enxerga
// as próprias transações; as de outros aparecem como inexistentes, para não
// revelar sua existência.
func (s *Service) GetTransaction(ctx context.Context, p Principal, id uuid.UUID) (*domain.WagerTransaction, error) {
	tx, err := s.store.Reads().Transactions().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !p.Internal && (p.ProviderID == "" || tx.ProviderID() != p.ProviderID) {
		return nil, ErrTransactionNotFound
	}
	return tx, nil
}

// GetTransactionByExternalID consulta por (providerId, externalTransactionId).
// Um provedor só pode consultar o próprio providerId.
func (s *Service) GetTransactionByExternalID(ctx context.Context, p Principal, providerID, externalID string) (*domain.WagerTransaction, error) {
	if !p.Internal && (p.ProviderID == "" || p.ProviderID != providerID) {
		return nil, fmt.Errorf("%w: provedor %q não pode consultar %q", ErrForbidden, p.ProviderID, providerID)
	}
	return s.store.Reads().Transactions().GetByExternalID(ctx, providerID, externalID)
}

// Reconciliation compara o saldo armazenado com o saldo reconstruído pelo ledger.
type Reconciliation struct {
	WalletID          uuid.UUID
	StoredBalance     domain.Money
	CalculatedBalance domain.Money
	Difference        domain.Money // armazenado - reconstruído
	Consistent        bool
	CheckedEntries    int64
}

// Reconcile reconstrói o saldo a partir do ledger (inclusive a abertura) e o
// compara com o saldo armazenado, em um único snapshot consistente. Não altera nada.
func (s *Service) Reconcile(ctx context.Context, p Principal, walletID uuid.UUID) (Reconciliation, error) {
	if err := requireInternal(p); err != nil {
		return Reconciliation{}, err
	}
	var rec Reconciliation
	err := s.store.ReadConsistent(ctx, func(ctx context.Context, r Repos) error {
		w, err := r.Wallets().Get(ctx, walletID)
		if err != nil {
			return err
		}
		net, count, err := r.Ledger().Summarize(ctx, walletID)
		if err != nil {
			return err
		}
		calculated, err := domain.NewMoneyFromMinor(net, w.Currency())
		if err != nil {
			return err
		}
		diff, err := w.Balance().Sub(calculated)
		if err != nil {
			return err
		}
		rec = Reconciliation{
			WalletID:          walletID,
			StoredBalance:     w.Balance(),
			CalculatedBalance: calculated,
			Difference:        diff,
			Consistent:        diff.IsZero(),
			CheckedEntries:    count,
		}
		return nil
	})
	return rec, err
}
