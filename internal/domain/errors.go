// Package domain contém o modelo financeiro: valores, carteiras, transações
// e lançamentos. Não depende de HTTP, SQS, Fx nem de bibliotecas de persistência.
package domain

import "errors"

// Erros de valor monetário. Use errors.Is para classificá-los; as mensagens
// detalhadas são adicionadas com fmt.Errorf("%w: ...").
var (
	ErrInvalidCurrency  = errors.New("invalid currency")
	ErrInvalidAmount    = errors.New("invalid amount")
	ErrNegativeAmount   = errors.New("negative amount")
	ErrAmountOverflow   = errors.New("amount overflow")
	ErrCurrencyMismatch = errors.New("currency mismatch")
	ErrUninitialized    = errors.New("uninitialized value")
)

// Erros de carteira e ledger.
var (
	ErrInvalidWallet      = errors.New("invalid wallet")
	ErrInvalidLedgerEntry = errors.New("invalid ledger entry")
	ErrNonPositiveAmount  = errors.New("amount must be positive")
	ErrInsufficientFunds  = errors.New("insufficient funds")
	ErrVersionOverflow    = errors.New("wallet version overflow")
)

// Erros de transação. São erros de entrada ou de programação: a operação não
// chega a ser persistida. Rejeições de negócio não são erros Go; elas viram
// uma transação REJECTED com FailureCode.
var (
	ErrInvalidTransaction = errors.New("invalid transaction")
	ErrInvalidKind        = errors.New("invalid transaction kind")
	ErrOpeningNotAllowed  = errors.New("OPENING is internal and cannot be submitted")
	ErrInvalidTransition  = errors.New("invalid status transition")
)
