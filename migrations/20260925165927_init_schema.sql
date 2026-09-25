-- +goose Up

-- Valores monetários são sempre BIGINT em unidades mínimas (centavos para BRL).
-- Nenhuma coluna financeira usa NUMERIC de ponto flutuante, REAL ou DOUBLE PRECISION.

-- ============================================================================
-- wallets: raiz do agregado financeiro
-- ============================================================================
CREATE TABLE wallets (
    id            UUID        PRIMARY KEY,
    player_id     UUID        NOT NULL,
    currency      CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_minor BIGINT      NOT NULL CHECK (balance_minor >= 0),
    version       BIGINT      NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,

    -- Uma única carteira por (jogador, moeda).
    CONSTRAINT wallets_player_currency_uk UNIQUE (player_id, currency),
    -- Alvo da FK composta do ledger, que garante a moeda da carteira.
    CONSTRAINT wallets_id_currency_uk UNIQUE (id, currency)
);

-- +goose StatementBegin
-- Identidade é imutável; a versão sobe exatamente 1 quando o saldo muda e
-- fica igual quando não muda. Uma escrita baseada em versão antiga é recusada.
CREATE FUNCTION wallets_guard_update() RETURNS trigger AS $$
BEGIN
    IF NEW.id <> OLD.id OR NEW.player_id <> OLD.player_id
       OR NEW.currency <> OLD.currency OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'wallets: campos de identidade são imutáveis'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.balance_minor <> OLD.balance_minor AND NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'wallets: mudança de saldo exige version = % (recebido %)', OLD.version + 1, NEW.version
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.balance_minor = OLD.balance_minor AND NEW.version <> OLD.version THEN
        RAISE EXCEPTION 'wallets: version só muda quando o saldo muda'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER wallets_guard_update
BEFORE UPDATE ON wallets
FOR EACH ROW EXECUTE FUNCTION wallets_guard_update();

-- +goose StatementBegin
CREATE FUNCTION forbid_delete() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '%: % não permitido', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER wallets_forbid_delete
BEFORE DELETE ON wallets
FOR EACH ROW EXECUTE FUNCTION forbid_delete();

-- ============================================================================
-- wager_transactions: operações internas (OPENING) e externas
-- ============================================================================
CREATE TABLE wager_transactions (
    id           UUID        PRIMARY KEY,
    kind         TEXT        NOT NULL CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    status       TEXT        NOT NULL CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),

    wallet_id    UUID        NOT NULL,
    player_id    UUID        NOT NULL,
    currency     CHAR(3)     NOT NULL,
    amount_minor BIGINT      NOT NULL CHECK (amount_minor >= 0),

    -- Metadados externos (NULL em OPENING).
    provider_id                       TEXT,
    external_transaction_id           TEXT,
    idempotency_key                   TEXT,
    payload_hash                      TEXT CHECK (payload_hash ~ '^[0-9a-f]{64}$'), -- SHA-256 hex
    round_id                          TEXT,
    game_id                           TEXT,
    reference_external_transaction_id TEXT,
    reference_transaction_id          UUID REFERENCES wager_transactions (id),

    -- Resultado persistido, devolvido em replays.
    failure_code          TEXT,
    result_balance_minor  BIGINT CHECK (result_balance_minor >= 0),
    result_wallet_version BIGINT CHECK (result_wallet_version >= 1),

    -- Retomada durável de PENDING / PENDING_REFERENCE por qualquer instância.
    attempts        INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ,

    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    processed_at TIMESTAMPTZ,

    -- Só a carteira é FK: uma operação com jogador ou moeda divergentes da
    -- carteira precisa ser gravada como REJECTED para ser auditável e devolvida
    -- em replays. Movimentação de saldo em moeda errada continua impossível
    -- pela FK (wallet_id, currency) do ledger.
    CONSTRAINT wager_tx_wallet_fk FOREIGN KEY (wallet_id) REFERENCES wallets (id),

    -- OPENING é interno: sem nenhum metadado externo e sempre com valor positivo.
    CONSTRAINT wager_tx_opening_shape CHECK (
        kind <> 'OPENING' OR (
            provider_id IS NULL AND external_transaction_id IS NULL AND idempotency_key IS NULL
            AND payload_hash IS NULL AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL AND reference_transaction_id IS NULL
            AND amount_minor > 0
        )
    ),

    -- Operações externas exigem todos os identificadores.
    CONSTRAINT wager_tx_external_shape CHECK (
        kind = 'OPENING' OR (
            provider_id IS NOT NULL AND provider_id <> ''
            AND external_transaction_id IS NOT NULL AND external_transaction_id <> ''
            AND idempotency_key IS NOT NULL AND idempotency_key <> ''
            AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL AND round_id <> ''
            AND game_id IS NOT NULL AND game_id <> ''
        )
    ),

    -- Política de valores: LOSS = 0; os demais > 0.
    CONSTRAINT wager_tx_amount_policy CHECK (
        (kind = 'LOSS' AND amount_minor = 0) OR (kind <> 'LOSS' AND amount_minor > 0)
    ),

    -- Referências: obrigatória em REFUND/ROLLBACK, opcional em WIN, proibida no resto.
    CONSTRAINT wager_tx_reference_policy CHECK (
        CASE kind
            WHEN 'REFUND'   THEN reference_external_transaction_id IS NOT NULL
            WHEN 'ROLLBACK' THEN reference_external_transaction_id IS NOT NULL
            WHEN 'WIN'      THEN TRUE
            ELSE reference_external_transaction_id IS NULL AND reference_transaction_id IS NULL
        END
    ),

    -- Uma referência resolvida sempre corresponde a uma referência externa informada.
    CONSTRAINT wager_tx_resolved_reference CHECK (
        reference_transaction_id IS NULL OR reference_external_transaction_id IS NOT NULL
    ),

    -- PENDING_REFERENCE só existe para quem tem referência.
    CONSTRAINT wager_tx_pending_reference CHECK (
        status <> 'PENDING_REFERENCE'
        OR (reference_external_transaction_id IS NOT NULL AND reference_transaction_id IS NULL)
    ),

    -- Coerência entre estado e resultado.
    CONSTRAINT wager_tx_status_result CHECK (
        CASE status
            WHEN 'PROCESSED' THEN failure_code IS NULL AND result_balance_minor IS NOT NULL
                                  AND result_wallet_version IS NOT NULL AND processed_at IS NOT NULL
            WHEN 'REJECTED'  THEN failure_code IS NOT NULL AND processed_at IS NOT NULL
            WHEN 'FAILED'    THEN failure_code IS NOT NULL AND processed_at IS NOT NULL
            ELSE failure_code IS NULL AND processed_at IS NULL AND result_balance_minor IS NULL
        END
    )
);

-- Uma operação externa por (provedor, id externo), qualquer que seja a chave usada.
CREATE UNIQUE INDEX wager_tx_provider_external_uk
    ON wager_transactions (provider_id, external_transaction_id)
    WHERE kind <> 'OPENING';

-- Idempotency-Key única dentro do provedor.
CREATE UNIQUE INDEX wager_tx_provider_idempotency_uk
    ON wager_transactions (provider_id, idempotency_key)
    WHERE kind <> 'OPENING';

-- No máximo um crédito inicial por carteira.
CREATE UNIQUE INDEX wager_tx_single_opening_uk
    ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING';

-- Uma transação referenciada recebe no máximo uma reversão bem-sucedida,
-- seja REFUND ou ROLLBACK. Isso impede devolver duas vezes o mesmo débito
-- (REFUND + ROLLBACK da mesma BET). Um ROLLBACK de um REFUND referencia o
-- REFUND, não a BET, e por isso continua permitido.
CREATE UNIQUE INDEX wager_tx_single_reversal_uk
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

-- Worker de retomada: busca PENDING/PENDING_REFERENCE vencidos.
CREATE INDEX wager_tx_due_idx
    ON wager_transactions (next_attempt_at)
    WHERE status IN ('PENDING', 'PENDING_REFERENCE');

-- Ao chegar uma transação, encontra quem a aguardava como referência.
CREATE INDEX wager_tx_waiting_reference_idx
    ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE status = 'PENDING_REFERENCE';

CREATE INDEX wager_tx_wallet_idx ON wager_transactions (wallet_id, created_at);

-- +goose StatementBegin
-- Identidade e conteúdo de negócio são imutáveis; estados terminais são finais.
CREATE FUNCTION wager_transactions_guard_update() RETURNS trigger AS $$
BEGIN
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'wager_transactions: transação % está em estado terminal %', OLD.id, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.id <> OLD.id
       OR NEW.kind <> OLD.kind
       OR NEW.wallet_id <> OLD.wallet_id
       OR NEW.player_id <> OLD.player_id
       OR NEW.currency <> OLD.currency
       OR NEW.amount_minor <> OLD.amount_minor
       OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
       OR NEW.external_transaction_id IS DISTINCT FROM OLD.external_transaction_id
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
       OR NEW.round_id IS DISTINCT FROM OLD.round_id
       OR NEW.game_id IS DISTINCT FROM OLD.game_id
       OR NEW.reference_external_transaction_id IS DISTINCT FROM OLD.reference_external_transaction_id
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'wager_transactions: dados de negócio da transação % são imutáveis', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- A referência resolvida só pode ser definida uma vez.
    IF OLD.reference_transaction_id IS NOT NULL
       AND NEW.reference_transaction_id IS DISTINCT FROM OLD.reference_transaction_id THEN
        RAISE EXCEPTION 'wager_transactions: referência resolvida da transação % é imutável', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER wager_transactions_guard_update
BEFORE UPDATE ON wager_transactions
FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard_update();

CREATE TRIGGER wager_transactions_forbid_delete
BEFORE DELETE ON wager_transactions
FOR EACH ROW EXECUTE FUNCTION forbid_delete();

-- ============================================================================
-- wallet_ledger_entries: livro-razão append-only
-- ============================================================================
CREATE TABLE wallet_ledger_entries (
    id                   UUID        PRIMARY KEY,
    wallet_id            UUID        NOT NULL,
    transaction_id       UUID        NOT NULL REFERENCES wager_transactions (id),
    direction            TEXT        NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    currency             CHAR(3)     NOT NULL,
    amount_minor         BIGINT      NOT NULL CHECK (amount_minor > 0),
    balance_before_minor BIGINT      NOT NULL CHECK (balance_before_minor >= 0),
    balance_after_minor  BIGINT      NOT NULL CHECK (balance_after_minor >= 0),
    -- Versão da carteira após o lançamento: ordena o ledger de forma estável
    -- e serve de cursor de paginação.
    wallet_version       BIGINT      NOT NULL CHECK (wallet_version >= 1),
    created_at           TIMESTAMPTZ NOT NULL,

    CONSTRAINT ledger_wallet_currency_fk FOREIGN KEY (wallet_id, currency)
        REFERENCES wallets (id, currency),

    -- Um lançamento por transação em cada carteira.
    CONSTRAINT ledger_wallet_transaction_uk UNIQUE (wallet_id, transaction_id),

    -- Uma única mudança de saldo por versão da carteira.
    CONSTRAINT ledger_wallet_version_uk UNIQUE (wallet_id, wallet_version),

    -- balanceAfter = balanceBefore ± amount, conforme a direção.
    CONSTRAINT ledger_balance_arithmetic CHECK (
        (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
        OR (direction = 'DEBIT' AND balance_after_minor = balance_before_minor - amount_minor)
    )
);

-- +goose StatementBegin
CREATE FUNCTION ledger_forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries é append-only: % não permitido', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER ledger_forbid_update_delete
BEFORE UPDATE OR DELETE ON wallet_ledger_entries
FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();

CREATE TRIGGER ledger_forbid_truncate
BEFORE TRUNCATE ON wallet_ledger_entries
FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();

-- ============================================================================
-- inbox_messages: deduplicação durável de mensagens consumidas
-- ============================================================================
CREATE TABLE inbox_messages (
    consumer_name  TEXT        NOT NULL CHECK (consumer_name <> ''),
    message_id     TEXT        NOT NULL CHECK (message_id <> ''),
    payload_hash   TEXT        NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    transaction_id UUID        REFERENCES wager_transactions (id),
    received_at    TIMESTAMPTZ NOT NULL,
    processed_at   TIMESTAMPTZ,

    CONSTRAINT inbox_messages_pk PRIMARY KEY (consumer_name, message_id)
);

-- +goose StatementBegin
CREATE FUNCTION inbox_messages_guard_update() RETURNS trigger AS $$
BEGIN
    IF NEW.consumer_name <> OLD.consumer_name OR NEW.message_id <> OLD.message_id
       OR NEW.payload_hash <> OLD.payload_hash OR NEW.received_at <> OLD.received_at THEN
        RAISE EXCEPTION 'inbox_messages: identidade da mensagem é imutável'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.processed_at IS NOT NULL
       AND (NEW.processed_at IS DISTINCT FROM OLD.processed_at
            OR NEW.transaction_id IS DISTINCT FROM OLD.transaction_id) THEN
        RAISE EXCEPTION 'inbox_messages: mensagem % já concluída', OLD.message_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER inbox_messages_guard_update
BEFORE UPDATE ON inbox_messages
FOR EACH ROW EXECUTE FUNCTION inbox_messages_guard_update();

-- ============================================================================
-- outbox_events: transactional outbox
-- ============================================================================
CREATE TABLE outbox_events (
    id              UUID        PRIMARY KEY, -- eventId estável, preservado em republicações
    aggregate_type  TEXT        NOT NULL CHECK (aggregate_type <> ''),
    aggregate_id    UUID        NOT NULL,
    event_type      TEXT        NOT NULL CHECK (event_type IN (
                        'WagerTransactionProcessed',
                        'WagerTransactionRejected',
                        'WalletBalanceChanged',
                        'WagerTransactionPendingReference'
                    )),
    event_version   INTEGER     NOT NULL CHECK (event_version >= 1),
    correlation_id  TEXT        NOT NULL CHECK (correlation_id <> ''),
    causation_id    TEXT,
    payload         JSONB       NOT NULL, -- snapshot imutável do envelope completo
    occurred_at     TIMESTAMPTZ NOT NULL,

    -- Controle de publicação.
    attempts        INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    locked_by       TEXT,
    locked_until    TIMESTAMPTZ, -- lease: expirado, outro publisher assume
    last_error      TEXT,
    published_at    TIMESTAMPTZ,

    CONSTRAINT outbox_lock_pair CHECK ((locked_by IS NULL) = (locked_until IS NULL))
);

-- Publishers buscam pendentes vencidos com FOR UPDATE SKIP LOCKED.
CREATE INDEX outbox_events_due_idx
    ON outbox_events (next_attempt_at)
    WHERE published_at IS NULL;

CREATE INDEX outbox_events_aggregate_idx ON outbox_events (aggregate_id, occurred_at);

-- +goose StatementBegin
-- O evento é imutável; só os campos de controle de publicação mudam,
-- e um evento publicado não volta a ser pendente.
CREATE FUNCTION outbox_events_guard_update() RETURNS trigger AS $$
BEGIN
    IF NEW.id <> OLD.id
       OR NEW.aggregate_type <> OLD.aggregate_type
       OR NEW.aggregate_id <> OLD.aggregate_id
       OR NEW.event_type <> OLD.event_type
       OR NEW.event_version <> OLD.event_version
       OR NEW.correlation_id <> OLD.correlation_id
       OR NEW.causation_id IS DISTINCT FROM OLD.causation_id
       OR NEW.payload <> OLD.payload
       OR NEW.occurred_at <> OLD.occurred_at THEN
        RAISE EXCEPTION 'outbox_events: evento % é imutável', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'outbox_events: evento % já publicado', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER outbox_events_guard_update
BEFORE UPDATE ON outbox_events
FOR EACH ROW EXECUTE FUNCTION outbox_events_guard_update();

-- +goose Down

DROP TABLE IF EXISTS outbox_events;
DROP FUNCTION IF EXISTS outbox_events_guard_update();

DROP TABLE IF EXISTS inbox_messages;
DROP FUNCTION IF EXISTS inbox_messages_guard_update();

DROP TABLE IF EXISTS wallet_ledger_entries;
DROP FUNCTION IF EXISTS ledger_forbid_mutation();

DROP TABLE IF EXISTS wager_transactions;
DROP FUNCTION IF EXISTS wager_transactions_guard_update();

DROP TABLE IF EXISTS wallets;
DROP FUNCTION IF EXISTS wallets_guard_update();
DROP FUNCTION IF EXISTS forbid_delete();
