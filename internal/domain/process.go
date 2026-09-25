package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Outcome é o resultado do processamento de uma operação.
type Outcome string

const (
	OutcomeProcessed         Outcome = "PROCESSED"
	OutcomeRejected          Outcome = "REJECTED"
	OutcomeAwaitingReference Outcome = "AWAITING_REFERENCE"
)

// ProcessInput reúne o que a regra de negócio precisa. O caso de uso carrega
// a carteira com lock e resolve a referência antes de chamar ProcessWager.
type ProcessInput struct {
	Transaction *WagerTransaction
	Wallet      *Wallet

	// Reference é a transação apontada por referenceExternalTransactionId,
	// buscada por (providerId, referenceExternalTransactionId). nil se não existe.
	Reference *WagerTransaction

	// ReferenceReversed informa se Reference já tem uma reversão PROCESSED.
	ReferenceReversed bool

	LedgerEntryID uuid.UUID
	RetryPolicy   ReferenceRetryPolicy
	Now           time.Time
}

// ProcessResult descreve o efeito do processamento. LedgerEntry só existe
// quando o saldo mudou.
type ProcessResult struct {
	Outcome     Outcome
	LedgerEntry *LedgerEntry
}

// ProcessWager aplica as regras de BET, WIN, LOSS, REFUND e ROLLBACK,
// alterando a transação e, quando há movimentação, a carteira.
//
// Rejeições de negócio não são erros: a transação termina REJECTED com um
// FailureCode. O erro retornado indica uso incorreto (transação terminal,
// carteira errada) ou falha inesperada, e nada deve ser persistido.
func ProcessWager(in ProcessInput) (ProcessResult, error) {
	tx, w := in.Transaction, in.Wallet
	if tx == nil || w == nil {
		return ProcessResult{}, fmt.Errorf("%w: transação e carteira são obrigatórias", ErrUninitialized)
	}
	if !tx.IsExternal() {
		return ProcessResult{}, fmt.Errorf("%w: OPENING não é processado por ProcessWager", ErrInvalidTransaction)
	}
	if tx.Status().IsTerminal() {
		return ProcessResult{}, fmt.Errorf("%w: transação %s já está %s", ErrInvalidTransition, tx.ID(), tx.Status())
	}
	if tx.WalletID() != w.ID() {
		return ProcessResult{}, fmt.Errorf("%w: carteira carregada não é a da transação", ErrInvalidTransaction)
	}

	p := processor{in: in, tx: tx, w: w}

	if tx.PlayerID() != w.PlayerID() {
		return p.reject(FailureWalletPlayerMismatch)
	}
	if tx.Money().Currency() != w.Currency() {
		return p.reject(FailureCurrencyMismatch)
	}

	if tx.HasReference() {
		if result, done, err := p.checkReference(); done || err != nil {
			return result, err
		}
	}

	switch tx.Kind() {
	case KindBet:
		return p.debit(FailureInsufficientFunds)
	case KindWin, KindRefund:
		return p.credit()
	case KindLoss:
		return p.processWithoutMovement()
	case KindRollback:
		if in.Reference.Kind() == KindBet {
			return p.credit()
		}
		return p.debit(FailureInsufficientFundsForReversal)
	default:
		return ProcessResult{}, fmt.Errorf("%w: %s", ErrInvalidKind, tx.Kind())
	}
}

type processor struct {
	in ProcessInput
	tx *WagerTransaction
	w  *Wallet
}

// checkReference valida a referência. done=true quando a operação já foi
// decidida (rejeitada ou aguardando).
func (p processor) checkReference() (ProcessResult, bool, error) {
	ref := p.in.Reference
	if ref == nil {
		r, err := p.await()
		return r, true, err
	}

	switch ref.Status() {
	case StatusPending, StatusPendingReference:
		r, err := p.await()
		return r, true, err
	case StatusRejected, StatusFailed:
		r, err := p.reject(FailureReferenceNotProcessed)
		return r, true, err
	}

	if err := p.tx.resolveReference(ref); err != nil {
		return ProcessResult{}, true, err
	}

	if !allowedReferenceKind(p.tx.Kind(), ref.Kind()) {
		r, err := p.reject(FailureInvalidReferenceKind)
		return r, true, err
	}
	if ref.ProviderID() != p.tx.ProviderID() || ref.PlayerID() != p.tx.PlayerID() ||
		ref.WalletID() != p.tx.WalletID() || ref.Money().Currency() != p.tx.Money().Currency() ||
		ref.RoundID() != p.tx.RoundID() {
		r, err := p.reject(FailureReferenceMismatch)
		return r, true, err
	}
	if p.tx.Kind().IsReversal() {
		if !ref.Money().Equal(p.tx.Money()) {
			r, err := p.reject(FailureReferenceAmountMismatch)
			return r, true, err
		}
		if p.in.ReferenceReversed {
			r, err := p.reject(FailureAlreadyReversed)
			return r, true, err
		}
	}
	return ProcessResult{}, false, nil
}

// allowedReferenceKind: WIN e REFUND referenciam uma BET; ROLLBACK desfaz
// uma BET, WIN ou REFUND.
func allowedReferenceKind(kind, refKind Kind) bool {
	switch kind {
	case KindWin, KindRefund:
		return refKind == KindBet
	case KindRollback:
		return refKind == KindBet || refKind == KindWin || refKind == KindRefund
	default:
		return false
	}
}

func (p processor) debit(insufficient FailureCode) (ProcessResult, error) {
	entry, err := p.w.Debit(p.in.LedgerEntryID, p.tx.ID(), p.tx.Money(), p.in.Now)
	if errors.Is(err, ErrInsufficientFunds) {
		return p.reject(insufficient)
	}
	if err != nil {
		return ProcessResult{}, err
	}
	return p.processed(&entry)
}

func (p processor) credit() (ProcessResult, error) {
	entry, err := p.w.Credit(p.in.LedgerEntryID, p.tx.ID(), p.tx.Money(), p.in.Now)
	if err != nil {
		return ProcessResult{}, err
	}
	return p.processed(&entry)
}

// processWithoutMovement conclui LOSS: sem lançamento e sem mudar a versão.
func (p processor) processWithoutMovement() (ProcessResult, error) {
	return p.processed(nil)
}

func (p processor) processed(entry *LedgerEntry) (ProcessResult, error) {
	if err := p.tx.markProcessed(p.w.Balance(), p.w.Version(), p.in.Now); err != nil {
		return ProcessResult{}, err
	}
	return ProcessResult{Outcome: OutcomeProcessed, LedgerEntry: entry}, nil
}

func (p processor) reject(code FailureCode) (ProcessResult, error) {
	if err := p.tx.markRejected(code, p.w.Balance(), p.w.Version(), p.in.Now); err != nil {
		return ProcessResult{}, err
	}
	return ProcessResult{Outcome: OutcomeRejected}, nil
}

func (p processor) await() (ProcessResult, error) {
	waiting, err := p.tx.awaitReference(p.in.RetryPolicy, p.in.Now)
	if err != nil {
		return ProcessResult{}, err
	}
	if !waiting {
		return ProcessResult{Outcome: OutcomeRejected}, nil
	}
	return ProcessResult{Outcome: OutcomeAwaitingReference}, nil
}

// OpenWalletResult contém a carteira aberta e, se o saldo inicial for
// positivo, a transação OPENING e seu lançamento de crédito.
type OpenWalletResult struct {
	Wallet      *Wallet
	Opening     *WagerTransaction
	LedgerEntry *LedgerEntry
}

// OpenWalletParams reúne os identificadores gerados pelo caso de uso.
type OpenWalletParams struct {
	WalletID       uuid.UUID
	PlayerID       uuid.UUID
	OpeningID      uuid.UUID
	LedgerEntryID  uuid.UUID
	InitialBalance Money
	Now            time.Time
}

// OpenWallet abre uma carteira. Com saldo inicial positivo, cria também o
// OPENING (PROCESSED) e o lançamento de crédito, a serem gravados no mesmo
// commit. Saldo inicial zero não cria OPENING nem lançamento.
func OpenWallet(p OpenWalletParams) (OpenWalletResult, error) {
	if !p.InitialBalance.IsValid() {
		return OpenWalletResult{}, fmt.Errorf("%w: saldo inicial", ErrUninitialized)
	}
	if p.InitialBalance.IsNegative() {
		return OpenWalletResult{}, fmt.Errorf("%w: saldo inicial %s", ErrNegativeAmount, p.InitialBalance)
	}
	w, err := NewWallet(p.WalletID, p.PlayerID, p.InitialBalance, p.Now)
	if err != nil {
		return OpenWalletResult{}, err
	}
	if p.InitialBalance.IsZero() {
		return OpenWalletResult{Wallet: w}, nil
	}

	opening, err := newOpeningTransaction(p.OpeningID, w)
	if err != nil {
		return OpenWalletResult{}, err
	}
	entry, err := w.OpeningEntry(p.LedgerEntryID, opening.ID())
	if err != nil {
		return OpenWalletResult{}, err
	}
	return OpenWalletResult{Wallet: w, Opening: opening, LedgerEntry: &entry}, nil
}
