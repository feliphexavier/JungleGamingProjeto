package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
)

type ledgerRepo struct{ db dbtx }

func (r ledgerRepo) Insert(ctx context.Context, e domain.LedgerEntry) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO wallet_ledger_entries
		       (id, wallet_id, transaction_id, direction, currency, amount_minor,
		        balance_before_minor, balance_after_minor, wallet_version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.ID(), e.WalletID(), e.TransactionID(), string(e.Direction()), e.Amount().Currency().String(),
		e.Amount().Minor(), e.BalanceBefore().Minor(), e.BalanceAfter().Minor(), e.WalletVersion(), e.CreatedAt())
	return mapError(err)
}

func (r ledgerRepo) List(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) ([]domain.LedgerEntry, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, wallet_id, transaction_id, direction, currency, amount_minor,
		       balance_before_minor, balance_after_minor, wallet_version, created_at
		  FROM wallet_ledger_entries
		 WHERE wallet_id = $1 AND wallet_version > $2
		 ORDER BY wallet_version
		 LIMIT $3`, walletID, afterVersion, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var entries []domain.LedgerEntry
	for rows.Next() {
		var (
			p                     domain.LedgerEntryParams
			direction, currency   string
			amount, before, after int64
		)
		if err := rows.Scan(&p.ID, &p.WalletID, &p.TransactionID, &direction, &currency, &amount,
			&before, &after, &p.WalletVersion, &p.CreatedAt); err != nil {
			return nil, mapError(err)
		}
		cur, err := domain.ParseCurrency(currency)
		if err != nil {
			return nil, err
		}
		p.Direction = domain.Direction(direction)
		if p.Amount, err = domain.NewMoneyFromMinor(amount, cur); err != nil {
			return nil, err
		}
		if p.BalanceBefore, err = domain.NewMoneyFromMinor(before, cur); err != nil {
			return nil, err
		}
		if p.BalanceAfter, err = domain.NewMoneyFromMinor(after, cur); err != nil {
			return nil, err
		}
		e, err := domain.RehydrateLedgerEntry(p)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, mapError(rows.Err())
}

func (r ledgerRepo) Summarize(ctx context.Context, walletID uuid.UUID) (int64, int64, error) {
	var net, count int64
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE direction WHEN 'CREDIT' THEN amount_minor ELSE -amount_minor END), 0)::BIGINT,
		       COUNT(*)
		  FROM wallet_ledger_entries
		 WHERE wallet_id = $1`, walletID).Scan(&net, &count)
	return net, count, mapError(err)
}
