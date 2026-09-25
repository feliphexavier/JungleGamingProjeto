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
