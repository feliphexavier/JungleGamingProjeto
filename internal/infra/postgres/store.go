// Package postgres implementa a persistência com pgx e SQL explícito.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
)

// dbtx é o que os repositórios precisam; atendido por pgx.Tx e pelo pool.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Connect abre o pool e confirma a conexão.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: configuração inválida: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", mapError(err))
	}
	return pool, nil
}

// Store implementa app.Store sobre um pool pgx.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var _ app.Store = (*Store)(nil)

// InTx executa fn em uma transação READ COMMITTED. Os repositórios entregues a
// fn compartilham a transação; commit só se fn retornar nil.
//
// READ COMMITTED basta porque a coordenação é explícita: a carteira é travada
// com SELECT ... FOR UPDATE, e cada comando posterior enxerga o que outras
// transações commitaram antes de o lock ser obtido.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context, r app.Repos) error) error {
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		return fn(ctx, repos{db: tx})
	})
	return mapError(err)
}

// ReadConsistent executa fn em uma transação somente leitura REPEATABLE READ:
// todas as consultas veem o mesmo snapshot.
func (s *Store) ReadConsistent(ctx context.Context, fn func(ctx context.Context, r app.Repos) error) error {
	opts := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	err := pgx.BeginTxFunc(ctx, s.pool, opts, func(tx pgx.Tx) error {
		return fn(ctx, repos{db: tx})
	})
	return mapError(err)
}

// Reads devolve repositórios sobre o pool, fora de transação.
func (s *Store) Reads() app.Repos { return repos{db: s.pool} }

// Ping verifica a disponibilidade do banco (readiness).
func (s *Store) Ping(ctx context.Context) error { return mapError(s.pool.Ping(ctx)) }

type repos struct{ db dbtx }

func (r repos) Wallets() app.WalletRepository           { return walletRepo(r) }
func (r repos) Transactions() app.TransactionRepository { return transactionRepo(r) }
func (r repos) Ledger() app.LedgerRepository            { return ledgerRepo(r) }
func (r repos) Outbox() app.OutboxRepository            { return outboxRepo(r) }
func (r repos) Inbox() app.InboxRepository              { return inboxRepo(r) }

// Códigos SQLSTATE relevantes.
const (
	codeUniqueViolation      = "23505"
	codeSerializationFailure = "40001"
	codeDeadlockDetected     = "40P01"
	codeLockNotAvailable     = "55P03"
)

// mapError traduz erros do pgx para os erros da aplicação:
//   - violação de unicidade → app.ErrDuplicate (a constraint vai na mensagem);
//   - deadlock e falha de serialização → app.ErrConcurrentUpdate;
//   - conexão, indisponibilidade e timeout → app.ErrUnavailable.
//
// Erros que já são da aplicação passam intactos.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{
		app.ErrDuplicate, app.ErrConcurrentUpdate, app.ErrUnavailable, app.ErrInvalidInput,
		app.ErrForbidden, app.ErrIdempotencyConflict, app.ErrInboxConflict, app.ErrWalletNotFound,
		app.ErrWalletAlreadyExists, app.ErrTransactionNotFound,
	} {
		if errors.Is(err, known) {
			return err
		}
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == codeUniqueViolation:
			return fmt.Errorf("%w: %s", app.ErrDuplicate, pgErr.ConstraintName)
		case pgErr.Code == codeSerializationFailure, pgErr.Code == codeDeadlockDetected, pgErr.Code == codeLockNotAvailable:
			return fmt.Errorf("%w: %s", app.ErrConcurrentUpdate, pgErr.Message)
		case len(pgErr.Code) == 2+3 && (pgErr.Code[:2] == "08" || pgErr.Code[:2] == "53" || pgErr.Code[:2] == "57"):
			// 08: conexão; 53: recursos insuficientes; 57: intervenção do operador
			// (inclui shutdown do servidor e cancelamento por timeout).
			return fmt.Errorf("%w: %s", app.ErrUnavailable, pgErr.Message)
		}
		return err
	}

	var netErr net.Error
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) || errors.As(err, &netErr) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) ||
		pgconn.SafeToRetry(err) || pgconn.Timeout(err) {
		return fmt.Errorf("%w: %w", app.ErrUnavailable, err)
	}
	return err
}
