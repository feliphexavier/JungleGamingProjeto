package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
)

type outboxRepo struct{ db dbtx }

// Insert grava os eventos na mesma transação da mudança que os originou. O
// payload é o envelope completo serializado, um snapshot imutável.
func (r outboxRepo) Insert(ctx context.Context, events []domain.Event) error {
	for _, ev := range events {
		payload, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("outbox: serializar %s: %w", ev.Type(), err)
		}
		_, err = r.db.Exec(ctx, `
			INSERT INTO outbox_events
			       (id, aggregate_type, aggregate_id, event_type, event_version, correlation_id,
			        causation_id, message_group_id, payload, occurred_at, next_attempt_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)`,
			ev.ID(), ev.AggregateType(), ev.AggregateID(), ev.Type(), ev.Version(), ev.CorrelationID(),
			nullString(ev.CausationID()), ev.WalletID().String(), payload, ev.OccurredAt())
		if err != nil {
			return mapError(err)
		}
	}
	return nil
}

type inboxRepo struct{ db dbtx }

// Register usa ON CONFLICT DO NOTHING: se outra transação estiver inserindo a
// mesma mensagem, esta espera o resultado dela, então duas entregas simultâneas
// nunca passam juntas.
func (r inboxRepo) Register(ctx context.Context, m app.InboxMessage, receivedAt time.Time) (app.InboxRecord, bool, error) {
	tag, err := r.db.Exec(ctx, `
		INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, received_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (consumer_name, message_id) DO NOTHING`,
		m.ConsumerName, m.MessageID, m.PayloadHash, receivedAt)
	if err != nil {
		return app.InboxRecord{}, false, mapError(err)
	}
	if tag.RowsAffected() == 1 {
		return app.InboxRecord{}, true, nil
	}

	var (
		rec         app.InboxRecord
		txID        *uuid.UUID
		processedAt *time.Time
	)
	err = r.db.QueryRow(ctx, `
		SELECT payload_hash, transaction_id, processed_at
		  FROM inbox_messages
		 WHERE consumer_name = $1 AND message_id = $2
		   FOR UPDATE`, m.ConsumerName, m.MessageID).Scan(&rec.PayloadHash, &txID, &processedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.InboxRecord{}, false, fmt.Errorf("%w: inbox %s desapareceu", app.ErrConcurrentUpdate, m.MessageID)
	}
	if err != nil {
		return app.InboxRecord{}, false, mapError(err)
	}
	if txID != nil {
		rec.TransactionID = *txID
	}
	rec.Processed = processedAt != nil
	return rec, false, nil
}

func (r inboxRepo) MarkProcessed(ctx context.Context, m app.InboxMessage, transactionID uuid.UUID, at time.Time) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE inbox_messages SET processed_at = $3, transaction_id = $4
		 WHERE consumer_name = $1 AND message_id = $2 AND processed_at IS NULL`,
		m.ConsumerName, m.MessageID, at, transactionID)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: inbox %s já concluída", app.ErrConcurrentUpdate, m.MessageID)
	}
	return nil
}
