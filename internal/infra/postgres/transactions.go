package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
)

type transactionRepo struct{ db dbtx }

const transactionColumns = `
	id, kind, status, wallet_id, player_id, currency, amount_minor,
	provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
	reference_external_transaction_id, reference_transaction_id,
	failure_code, result_balance_minor, result_wallet_version,
	attempts, next_attempt_at, created_at, updated_at, processed_at`

func (r transactionRepo) Insert(ctx context.Context, t domain.TransactionSnapshot) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO wager_transactions (`+transactionColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
		        $16, $17, $18, $19, $20, $21, $22, $23)`,
		t.ID, string(t.Kind), string(t.Status), t.WalletID, t.PlayerID, t.Money.Currency().String(), t.Money.Minor(),
		nullString(t.ProviderID), nullString(t.ExternalTransactionID), nullString(t.IdempotencyKey),
		nullString(t.PayloadHash), nullString(t.RoundID), nullString(t.GameID),
		nullString(t.ReferenceExternalTransactionID), nullUUID(t.ReferenceTransactionID),
		nullString(string(t.FailureCode)), nullMinor(t.ResultBalance), nullVersion(t.ResultWalletVersion),
		t.Attempts, nullTime(t.NextAttemptAt), t.CreatedAt, t.UpdatedAt, nullTime(t.ProcessedAt))
	return mapError(err)
}

func (r transactionRepo) Update(ctx context.Context, t domain.TransactionSnapshot) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE wager_transactions
		   SET status = $2,
		       reference_transaction_id = $3,
		       failure_code = $4,
		       result_balance_minor = $5,
		       result_wallet_version = $6,
		       attempts = $7,
		       next_attempt_at = $8,
		       updated_at = $9,
		       processed_at = $10
		 WHERE id = $1`,
		t.ID, string(t.Status), nullUUID(t.ReferenceTransactionID), nullString(string(t.FailureCode)),
		nullMinor(t.ResultBalance), nullVersion(t.ResultWalletVersion), t.Attempts,
		nullTime(t.NextAttemptAt), t.UpdatedAt, nullTime(t.ProcessedAt))
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: %s", app.ErrTransactionNotFound, t.ID)
	}
	return nil
}

func (r transactionRepo) Get(ctx context.Context, id uuid.UUID) (*domain.WagerTransaction, error) {
	return r.getOne(ctx, `SELECT `+transactionColumns+` FROM wager_transactions WHERE id = $1`, id)
}

func (r transactionRepo) GetByIdempotencyKey(ctx context.Context, providerID, key string) (*domain.WagerTransaction, error) {
	return r.getOne(ctx, `
		SELECT `+transactionColumns+` FROM wager_transactions
		 WHERE provider_id = $1 AND idempotency_key = $2 AND kind <> 'OPENING'`, providerID, key)
}

func (r transactionRepo) GetByExternalID(ctx context.Context, providerID, externalID string) (*domain.WagerTransaction, error) {
	return r.getOne(ctx, `
		SELECT `+transactionColumns+` FROM wager_transactions
		 WHERE provider_id = $1 AND external_transaction_id = $2 AND kind <> 'OPENING'`, providerID, externalID)
}

func (r transactionRepo) HasProcessedReversal(ctx context.Context, referenceID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM wager_transactions
			 WHERE reference_transaction_id = $1
			   AND status = 'PROCESSED'
			   AND kind IN ('REFUND', 'ROLLBACK'))`, referenceID).Scan(&exists)
	return exists, mapError(err)
}

// ClaimDuePendingReferences trava as linhas selecionadas até o fim da
// transação. SKIP LOCKED faz outras instâncias pularem as já travadas.
func (r transactionRepo) ClaimDuePendingReferences(ctx context.Context, now time.Time, limit int) ([]*domain.WagerTransaction, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+transactionColumns+` FROM wager_transactions
		 WHERE status = 'PENDING_REFERENCE' AND next_attempt_at <= $1
		 ORDER BY next_attempt_at
		 LIMIT $2
		 FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var result []*domain.WagerTransaction
	for rows.Next() {
		tx, err := scanTransaction(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, tx)
	}
	return result, mapError(rows.Err())
}

// WakeWaitingFor antecipa para agora a próxima tentativa de quem aguarda a
// operação recém-processada. SKIP LOCKED evita deadlock com um worker que já
// esteja processando alguma delas (ele trava a transação e depois a carteira;
// aqui a carteira já está travada).
func (r transactionRepo) WakeWaitingFor(ctx context.Context, providerID, externalID string, now time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE wager_transactions SET next_attempt_at = $3
		 WHERE id IN (
			SELECT id FROM wager_transactions
			 WHERE status = 'PENDING_REFERENCE'
			   AND provider_id = $1
			   AND reference_external_transaction_id = $2
			   AND next_attempt_at > $3
			 FOR UPDATE SKIP LOCKED)`, providerID, externalID, now)
	return mapError(err)
}

func (r transactionRepo) getOne(ctx context.Context, sql string, args ...any) (*domain.WagerTransaction, error) {
	tx, err := scanTransaction(r.db.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, app.ErrTransactionNotFound
	}
	return tx, err
}

func scanTransaction(row pgx.Row) (*domain.WagerTransaction, error) {
	var (
		s                                                       domain.TransactionSnapshot
		kind, status, currency                                  string
		amount                                                  int64
		provider, external, key, hash, round, game, refExternal *string
		refID                                                   *uuid.UUID
		failure                                                 *string
		resultBalance, resultVersion                            *int64
		nextAttempt, processedAt                                *time.Time
	)
	err := row.Scan(&s.ID, &kind, &status, &s.WalletID, &s.PlayerID, &currency, &amount,
		&provider, &external, &key, &hash, &round, &game, &refExternal, &refID,
		&failure, &resultBalance, &resultVersion,
		&s.Attempts, &nextAttempt, &s.CreatedAt, &s.UpdatedAt, &processedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err != nil {
		return nil, mapError(err)
	}

	cur, err := domain.ParseCurrency(currency)
	if err != nil {
		return nil, err
	}
	if s.Money, err = domain.NewMoneyFromMinor(amount, cur); err != nil {
		return nil, err
	}
	if resultBalance != nil {
		if s.ResultBalance, err = domain.NewMoneyFromMinor(*resultBalance, cur); err != nil {
			return nil, err
		}
	}
	s.Kind = domain.Kind(kind)
	s.Status = domain.Status(status)
	s.ProviderID = deref(provider)
	s.ExternalTransactionID = deref(external)
	s.IdempotencyKey = deref(key)
	s.PayloadHash = deref(hash)
	s.RoundID = deref(round)
	s.GameID = deref(game)
	s.ReferenceExternalTransactionID = deref(refExternal)
	if refID != nil {
		s.ReferenceTransactionID = *refID
	}
	s.FailureCode = domain.FailureCode(deref(failure))
	if resultVersion != nil {
		s.ResultWalletVersion = *resultVersion
	}
	if nextAttempt != nil {
		s.NextAttemptAt = *nextAttempt
	}
	if processedAt != nil {
		s.ProcessedAt = *processedAt
	}
	return domain.RehydrateTransaction(s)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullMinor(m domain.Money) any {
	if !m.IsValid() {
		return nil
	}
	return m.Minor()
}

func nullVersion(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
