package postgres

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// outboxClaimLockKey serializa as reservas entre instâncias (lock de
// transação, liberado no commit). A reserva é uma consulta curta; a publicação
// acontece depois, em paralelo entre instâncias.
const outboxClaimLockKey int64 = 0x6f7574626f78 // "outbox"

// Claim reserva um prefixo contínuo dos pendentes de cada carteira:
//   - a carteira não pode ter reserva ativa (de qualquer instância), senão
//     duas instâncias publicariam eventos da mesma carteira fora de ordem;
//   - nenhum evento anterior da carteira pode estar aguardando nova tentativa;
//   - ORDER BY seq com LIMIT mantém o prefixo mesmo quando o lote corta a carteira.
func (r outboxRepo) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]app.OutboxMessage, error) {
	if _, err := r.db.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, outboxClaimLockKey); err != nil {
		return nil, mapError(err)
	}
	rows, err := r.db.Query(ctx, `
		WITH candidates AS (
			SELECT o.id
			  FROM outbox_events o
			 WHERE o.published_at IS NULL
			   AND o.next_attempt_at <= $2
			   AND NOT EXISTS (
			       SELECT 1 FROM outbox_events l
			        WHERE l.message_group_id = o.message_group_id
			          AND l.published_at IS NULL
			          AND l.locked_until > $2)
			   AND NOT EXISTS (
			       SELECT 1 FROM outbox_events p
			        WHERE p.message_group_id = o.message_group_id
			          AND p.published_at IS NULL
			          AND p.seq < o.seq
			          AND p.next_attempt_at > $2)
			 ORDER BY o.seq
			 LIMIT $4
			   FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_events e
		   SET locked_by = $1, locked_until = $3
		  FROM candidates c
		 WHERE e.id = c.id
		RETURNING e.id, e.message_group_id, e.event_type, e.payload, e.attempts, e.seq`,
		owner, now, now.Add(lease), limit)
	if err != nil {
		return nil, mapError(err)
	}
	msgs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.OutboxMessage, error) {
		var m app.OutboxMessage
		err := row.Scan(&m.ID, &m.GroupID, &m.EventType, &m.Payload, &m.Attempts, &m.Seq)
		return m, err
	})
	if err != nil {
		return nil, mapError(err)
	}
	slices.SortFunc(msgs, func(a, b app.OutboxMessage) int { return cmp.Compare(a.Seq, b.Seq) })
	return msgs, nil
}

func (r outboxRepo) MarkPublished(ctx context.Context, id uuid.UUID, owner string, at time.Time) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE outbox_events
		   SET published_at = $3, locked_by = NULL, locked_until = NULL, last_error = NULL
		 WHERE id = $1 AND locked_by = $2 AND published_at IS NULL`, id, owner, at)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: reserva do evento %s perdida", app.ErrConcurrentUpdate, id)
	}
	return nil
}

func (r outboxRepo) MarkFailed(ctx context.Context, id uuid.UUID, owner string, nextAttempt time.Time, reason string) error {
	if len(reason) > 1000 {
		reason = reason[:1000]
	}
	_, err := r.db.Exec(ctx, `
		UPDATE outbox_events
		   SET attempts = attempts + 1, next_attempt_at = $3, last_error = $4,
		       locked_by = NULL, locked_until = NULL
		 WHERE id = $1 AND locked_by = $2 AND published_at IS NULL`, id, owner, nextAttempt, reason)
	return mapError(err)
}

func (r outboxRepo) Release(ctx context.Context, ids []uuid.UUID, owner string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE outbox_events SET locked_by = NULL, locked_until = NULL
		 WHERE id = ANY($1) AND locked_by = $2 AND published_at IS NULL`, ids, owner)
	return mapError(err)
}
