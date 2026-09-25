// Package app contém os casos de uso. Orquestra o domínio e a persistência por
// meio de interfaces; não depende de pgx, HTTP, SQS nem Fx.
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
)

// Store delimita as transações SQL. Todo repositório recebido em fn usa a
// mesma transação: o commit acontece só se fn retornar nil, e qualquer erro
// faz rollback de tudo.
type Store interface {
	InTx(ctx context.Context, fn func(ctx context.Context, r Repos) error) error

	// ReadConsistent executa fn em uma transação somente leitura com snapshot
	// único (REPEATABLE READ), usada pela reconciliação.
	ReadConsistent(ctx context.Context, fn func(ctx context.Context, r Repos) error) error

	// Reads devolve repositórios fora de transação, para consultas simples.
	Reads() Repos
}

// Repos agrupa os repositórios ligados a uma mesma transação.
type Repos interface {
	Wallets() WalletRepository
	Transactions() TransactionRepository
	Ledger() LedgerRepository
	Outbox() OutboxRepository
	Inbox() InboxRepository
}

type WalletRepository interface {
	// Insert grava uma carteira nova. Retorna ErrWalletAlreadyExists se já
	// existir carteira para (jogador, moeda).
	Insert(ctx context.Context, w domain.WalletSnapshot) error

	// GetForUpdate carrega a carteira com lock exclusivo da linha até o fim da
	// transação. Retorna ErrWalletNotFound se não existir.
	GetForUpdate(ctx context.Context, id uuid.UUID) (*domain.Wallet, error)

	Get(ctx context.Context, id uuid.UUID) (*domain.Wallet, error)

	// UpdateBalance grava saldo e versão somente se a versão persistida for
	// expectedVersion. Retorna ErrConcurrentUpdate caso contrário.
	UpdateBalance(ctx context.Context, w domain.WalletSnapshot, expectedVersion int64) error
}

type TransactionRepository interface {
	// Insert grava uma transação nova. Retorna ErrDuplicate em violação de
	// unicidade (chave de idempotência, id externo, OPENING ou reversão).
	Insert(ctx context.Context, t domain.TransactionSnapshot) error

	// Update grava a nova situação de uma transação ainda não terminal.
	Update(ctx context.Context, t domain.TransactionSnapshot) error

	Get(ctx context.Context, id uuid.UUID) (*domain.WagerTransaction, error)
	GetByIdempotencyKey(ctx context.Context, providerID, key string) (*domain.WagerTransaction, error)
	GetByExternalID(ctx context.Context, providerID, externalID string) (*domain.WagerTransaction, error)

	// HasProcessedReversal informa se a transação já tem REFUND ou ROLLBACK PROCESSED.
	HasProcessedReversal(ctx context.Context, referenceID uuid.UUID) (bool, error)

	// ClaimDuePendingReferences trava (SKIP LOCKED) até limit transações em
	// PENDING_REFERENCE com próxima tentativa vencida.
	ClaimDuePendingReferences(ctx context.Context, now time.Time, limit int) ([]*domain.WagerTransaction, error)

	// WakeWaitingFor antecipa a próxima tentativa das transações que aguardam
	// (providerID, externalID) como referência.
	WakeWaitingFor(ctx context.Context, providerID, externalID string, now time.Time) error
}

type LedgerRepository interface {
	Insert(ctx context.Context, e domain.LedgerEntry) error

	// List devolve até limit lançamentos com wallet_version > afterVersion, em
	// ordem crescente de versão.
	List(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) ([]domain.LedgerEntry, error)

	// Summarize devolve a soma de créditos menos débitos e a quantidade de lançamentos.
	Summarize(ctx context.Context, walletID uuid.UUID) (netMinor int64, entries int64, err error)
}

type OutboxRepository interface {
	Insert(ctx context.Context, events []domain.Event) error
}

// InboxMessage identifica uma mensagem consumida.
type InboxMessage struct {
	ConsumerName string
	MessageID    string
	PayloadHash  string
}

// InboxRecord é o estado persistido de uma mensagem já recebida.
type InboxRecord struct {
	PayloadHash   string
	TransactionID uuid.UUID
	Processed     bool
}

type InboxRepository interface {
	// Register grava a mensagem se ela for nova. Se já existir, devolve o
	// registro existente e created=false.
	Register(ctx context.Context, m InboxMessage, receivedAt time.Time) (existing InboxRecord, created bool, err error)
	MarkProcessed(ctx context.Context, m InboxMessage, transactionID uuid.UUID, at time.Time) error
}

// Clock permite controlar o tempo nos testes.
type Clock interface {
	Now() time.Time
}

// SystemClock usa o relógio do sistema, em UTC.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// NewID gera identificadores UUIDv7 (ordenáveis por tempo, bons para índices).
func NewID() uuid.UUID { return uuid.Must(uuid.NewV7()) }
