package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
)

type walletRepo struct{ db dbtx }

const walletColumns = `id, player_id, currency, balance_minor, version, created_at, updated_at`

func (r walletRepo) Insert(ctx context.Context, w domain.WalletSnapshot) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO wallets (`+walletColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID, w.PlayerID, w.Balance.Currency().String(), w.Balance.Minor(), w.Version, w.CreatedAt, w.UpdatedAt)
	err = mapError(err)
	if errors.Is(err, app.ErrDuplicate) && strings.Contains(err.Error(), "wallets_player_currency_uk") {
		return fmt.Errorf("%w: jogador %s já tem carteira em %s", app.ErrWalletAlreadyExists, w.PlayerID, w.Balance.Currency())
	}
	return err
}

func (r walletRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*domain.Wallet, error) {
	return r.get(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, id)
}

func (r walletRepo) Get(ctx context.Context, id uuid.UUID) (*domain.Wallet, error) {
	return r.get(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, id)
}

func (r walletRepo) get(ctx context.Context, sql string, id uuid.UUID) (*domain.Wallet, error) {
	var (
		s        domain.WalletSnapshot
		currency string
		balance  int64
	)
	err := r.db.QueryRow(ctx, sql, id).Scan(&s.ID, &s.PlayerID, &currency, &balance, &s.Version, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", app.ErrWalletNotFound, id)
	}
	if err != nil {
		return nil, mapError(err)
	}
	cur, err := domain.ParseCurrency(currency)
	if err != nil {
		return nil, err
	}
	if s.Balance, err = domain.NewMoneyFromMinor(balance, cur); err != nil {
		return nil, err
	}
	return domain.RehydrateWallet(s)
}

// UpdateBalance usa a versão como condição (compare-and-set). Com o lock da
// linha isso nunca deveria falhar; se falhar, algum caminho escreveu sem lock
// e a atualização é recusada em vez de sobrescrever a outra.
func (r walletRepo) UpdateBalance(ctx context.Context, w domain.WalletSnapshot, expectedVersion int64) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE wallets
		   SET balance_minor = $2, version = $3, updated_at = $4
		 WHERE id = $1 AND version = $5`,
		w.ID, w.Balance.Minor(), w.Version, w.UpdatedAt, expectedVersion)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: carteira %s não está na versão %d", app.ErrConcurrentUpdate, w.ID, expectedVersion)
	}
	return nil
}
