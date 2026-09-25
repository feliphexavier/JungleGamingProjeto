-- +goose Up

-- Ordem de gravação dos eventos. Eventos da mesma carteira são gravados sob o
-- lock da carteira, então seq reflete a ordem em que as mudanças foram
-- commitadas; occurred_at não serve para isso (eventos da mesma transação
-- compartilham o instante).
ALTER TABLE outbox_events ADD COLUMN seq BIGINT GENERATED ALWAYS AS IDENTITY;

-- Busca do evento pendente mais antigo de cada carteira.
CREATE INDEX outbox_events_group_pending_idx
    ON outbox_events (message_group_id, seq)
    WHERE published_at IS NULL;

-- +goose Down

DROP INDEX IF EXISTS outbox_events_group_pending_idx;
ALTER TABLE outbox_events DROP COLUMN IF EXISTS seq;
