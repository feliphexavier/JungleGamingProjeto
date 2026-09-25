package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
)

// SubmitWagerCommand é uma operação recebida de um provedor, por HTTP ou SQS.
// Os campos chegam como texto e são validados aqui, para que os dois canais
// tenham exatamente as mesmas regras.
type SubmitWagerCommand struct {
	Principal      Principal
	IdempotencyKey string

	ProviderID                     string
	ExternalTransactionID          string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           string
	Amount                         string
	Currency                       string
	ReferenceExternalTransactionID string

	CorrelationID string
	CausationID   string

	// Inbox identifica a mensagem SQS de origem; nil para HTTP.
	Inbox *InboxMessage
}

// SubmitWagerResult devolve a transação persistida. Replay indica que a
// operação já existia e nada foi reaplicado.
type SubmitWagerResult struct {
	Transaction *domain.WagerTransaction
	Replay      bool
}

type wagerInput struct {
	fields domain.PayloadFields
	hash   string
}

func parseWagerCommand(cmd SubmitWagerCommand) (wagerInput, error) {
	invalid := func(err error) (wagerInput, error) {
		return wagerInput{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if cmd.IdempotencyKey == "" {
		return invalid(errors.New("idempotency key obrigatória"))
	}
	playerID, err := uuid.Parse(cmd.PlayerID)
	if err != nil {
		return invalid(fmt.Errorf("playerId: %w", err))
	}
	walletID, err := uuid.Parse(cmd.WalletID)
	if err != nil {
		return invalid(fmt.Errorf("walletId: %w", err))
	}
	kind, err := domain.ParseExternalKind(cmd.Kind)
	if err != nil {
		return invalid(err)
	}
	money, err := domain.ParseMoney(cmd.Amount, cmd.Currency)
	if err != nil {
		return invalid(err)
	}
	fields := domain.PayloadFields{
		ProviderID:                     cmd.ProviderID,
		ExternalTransactionID:          cmd.ExternalTransactionID,
		PlayerID:                       playerID,
		WalletID:                       walletID,
		RoundID:                        cmd.RoundID,
		GameID:                         cmd.GameID,
		Kind:                           kind,
		Money:                          money,
		ReferenceExternalTransactionID: cmd.ReferenceExternalTransactionID,
	}
	hash, err := domain.PayloadHash(fields)
	if err != nil {
		return invalid(err)
	}
	return wagerInput{fields: fields, hash: hash}, nil
}

// SubmitWager registra e processa uma operação de provedor. É o mesmo caminho
// para HTTP e SQS, com as mesmas garantias de idempotência.
//
// Toda a decisão acontece em uma transação SQL com a carteira travada
// (SELECT ... FOR UPDATE): operações da mesma carteira são serializadas entre
// todas as instâncias; carteiras diferentes seguem em paralelo.
func (s *Service) SubmitWager(ctx context.Context, cmd SubmitWagerCommand) (SubmitWagerResult, error) {
	in, err := parseWagerCommand(cmd)
	if err != nil {
		return SubmitWagerResult{}, err
	}
	// O providerId autorizado vem do token, nunca do corpo.
	if cmd.Principal.ProviderID == "" || cmd.Principal.ProviderID != in.fields.ProviderID {
		return SubmitWagerResult{}, fmt.Errorf("%w: provedor %q não autorizado para %q",
			ErrForbidden, cmd.Principal.ProviderID, in.fields.ProviderID)
	}

	// Atalho sem lock para reenvios HTTP: não disputa a carteira.
	if cmd.Inbox == nil {
		existing, err := findExisting(ctx, s.store.Reads(), in, cmd.IdempotencyKey)
		if err != nil || existing != nil {
			return SubmitWagerResult{Transaction: existing, Replay: existing != nil}, err
		}
	}

	var lastErr error
	for attempt := 0; attempt < maxRaceRetries; attempt++ {
		result, err := s.submitOnce(ctx, cmd, in)
		if errors.Is(err, ErrDuplicate) || errors.Is(err, ErrConcurrentUpdate) {
			// Outra instância gravou antes; na próxima volta o registro vencedor
			// é encontrado e vira replay ou conflito.
			lastErr = err
			continue
		}
		return result, err
	}
	return SubmitWagerResult{}, fmt.Errorf("%w: %w", ErrUnavailable, lastErr)
}

func (s *Service) submitOnce(ctx context.Context, cmd SubmitWagerCommand, in wagerInput) (SubmitWagerResult, error) {
	var result SubmitWagerResult
	err := s.store.InTx(ctx, func(ctx context.Context, r Repos) error {
		now := s.clock.Now()

		if cmd.Inbox != nil {
			rec, created, err := r.Inbox().Register(ctx, *cmd.Inbox, now)
			if err != nil {
				return err
			}
			if !created {
				if rec.PayloadHash != cmd.Inbox.PayloadHash {
					return fmt.Errorf("%w: messageId %s", ErrInboxConflict, cmd.Inbox.MessageID)
				}
				if rec.Processed {
					tx, err := r.Transactions().Get(ctx, rec.TransactionID)
					if err != nil {
						return err
					}
					result = SubmitWagerResult{Transaction: tx, Replay: true}
					return nil
				}
			}
		}

		wallet, err := r.Wallets().GetForUpdate(ctx, in.fields.WalletID)
		if err != nil {
			return err
		}

		// Com a carteira travada, uma operação concorrente da mesma carteira já
		// terminou e seu registro está visível.
		existing, err := findExisting(ctx, r, in, cmd.IdempotencyKey)
		if err != nil {
			return err
		}
		if existing != nil {
			result = SubmitWagerResult{Transaction: existing, Replay: true}
			return s.completeInbox(ctx, r, cmd.Inbox, existing.ID(), now)
		}

		tx, err := domain.NewExternalTransaction(domain.ExternalTransactionParams{
			ID:                             s.newID(),
			ProviderID:                     in.fields.ProviderID,
			ExternalTransactionID:          in.fields.ExternalTransactionID,
			IdempotencyKey:                 cmd.IdempotencyKey,
			PayloadHash:                    in.hash,
			PlayerID:                       in.fields.PlayerID,
			WalletID:                       in.fields.WalletID,
			RoundID:                        in.fields.RoundID,
			GameID:                         in.fields.GameID,
			Kind:                           in.fields.Kind,
			Money:                          in.fields.Money,
			ReferenceExternalTransactionID: in.fields.ReferenceExternalTransactionID,
			Now:                            now,
		})
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}

		if err := s.process(ctx, r, tx, wallet, true, eventMeta(cmd), now); err != nil {
			return err
		}
		result = SubmitWagerResult{Transaction: tx}
		return s.completeInbox(ctx, r, cmd.Inbox, tx.ID(), now)
	})
	return result, err
}

// process aplica as regras à transação já inserida e grava todos os efeitos
// (transação, saldo, lançamento, eventos) na transação SQL corrente.
func (s *Service) process(ctx context.Context, r Repos, tx *domain.WagerTransaction, wallet *domain.Wallet, isNew bool, meta domain.EventMeta, now time.Time) error {
	var ref *domain.WagerTransaction
	reversed := false
	if tx.HasReference() {
		found, err := r.Transactions().GetByExternalID(ctx, tx.ProviderID(), tx.ReferenceExternalTransactionID())
		if err != nil && !errors.Is(err, ErrTransactionNotFound) {
			return err
		}
		ref = found
		if ref != nil && tx.Kind().IsReversal() {
			if reversed, err = r.Transactions().HasProcessedReversal(ctx, ref.ID()); err != nil {
				return err
			}
		}
	}

	versionBefore := wallet.Version()
	outcome, err := domain.ProcessWager(domain.ProcessInput{
		Transaction:       tx,
		Wallet:            wallet,
		Reference:         ref,
		ReferenceReversed: reversed,
		LedgerEntryID:     s.newID(),
		RetryPolicy:       s.retryPolicy,
		Now:               now,
	})
	if err != nil {
		return err
	}

	// Operação nova é inserida já no estado final (sem commit intermediário de
	// aceite); uma retomada atualiza a linha existente.
	save := r.Transactions().Update
	if isNew {
		save = r.Transactions().Insert
	}
	if err := save(ctx, tx.Snapshot()); err != nil {
		return err
	}
	if outcome.LedgerEntry != nil {
		if err := r.Wallets().UpdateBalance(ctx, wallet.Snapshot(), versionBefore); err != nil {
			return err
		}
		if err := r.Ledger().Insert(ctx, *outcome.LedgerEntry); err != nil {
			return err
		}
	}

	events, err := domain.TransactionEvents(tx, outcome.LedgerEntry, meta, s.newID)
	if err != nil {
		return err
	}
	if err := r.Outbox().Insert(ctx, events); err != nil {
		return err
	}

	// Operações que aguardavam esta como referência são antecipadas.
	if tx.Status() == domain.StatusProcessed {
		return r.Transactions().WakeWaitingFor(ctx, tx.ProviderID(), tx.ExternalTransactionID(), now)
	}
	return nil
}

func (s *Service) completeInbox(ctx context.Context, r Repos, m *InboxMessage, txID uuid.UUID, now time.Time) error {
	if m == nil {
		return nil
	}
	return r.Inbox().MarkProcessed(ctx, *m, txID, now)
}

// findExisting procura a operação pelo id externo e pela chave de
// idempotência. Devolve nil se não existir, ou ErrIdempotencyConflict se
// existir com outro conteúdo ou com outra chave.
//
// Cada conclusão compara os dados do próprio registro encontrado (chave e
// hash), e não o resultado da consulta anterior: fora de transação, outra
// instância pode commitar a mesma operação entre as duas consultas.
func findExisting(ctx context.Context, r Repos, in wagerInput, key string) (*domain.WagerTransaction, error) {
	tx, err := r.Transactions().GetByExternalID(ctx, in.fields.ProviderID, in.fields.ExternalTransactionID)
	switch {
	case err == nil:
		if tx.IdempotencyKey() != key {
			return nil, fmt.Errorf("%w: operação %q já registrada com outra chave",
				ErrIdempotencyConflict, in.fields.ExternalTransactionID)
		}
		return matchHash(tx, in, key)
	case !errors.Is(err, ErrTransactionNotFound):
		return nil, err
	}

	tx, err = r.Transactions().GetByIdempotencyKey(ctx, in.fields.ProviderID, key)
	switch {
	case err == nil:
		return matchHash(tx, in, key)
	case errors.Is(err, ErrTransactionNotFound):
		return nil, nil
	default:
		return nil, err
	}
}

func matchHash(tx *domain.WagerTransaction, in wagerInput, key string) (*domain.WagerTransaction, error) {
	if tx.PayloadHash() != in.hash {
		return nil, fmt.Errorf("%w: chave %q já usada com outro conteúdo", ErrIdempotencyConflict, key)
	}
	return tx, nil
}

func eventMeta(cmd SubmitWagerCommand) domain.EventMeta {
	meta := domain.EventMeta{CorrelationID: cmd.CorrelationID, CausationID: cmd.CausationID}
	if meta.CorrelationID == "" {
		meta.CorrelationID = cmd.IdempotencyKey
	}
	return meta
}
