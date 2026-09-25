package domain

// FailureCode é o código estável de uma transação REJECTED ou FAILED.
// Os valores fazem parte do contrato externo e não devem mudar.
type FailureCode string

const (
	// Rejeições definitivas: resultado de negócio; reenviar a mesma operação
	// devolve o mesmo resultado.
	FailureInsufficientFunds            FailureCode = "INSUFFICIENT_FUNDS"
	FailureInsufficientFundsForReversal FailureCode = "INSUFFICIENT_FUNDS_FOR_REVERSAL"
	FailureReferenceNotFound            FailureCode = "REFERENCE_NOT_FOUND"
	FailureReferenceNotProcessed        FailureCode = "REFERENCE_NOT_PROCESSED"
	FailureAlreadyReversed              FailureCode = "ALREADY_REVERSED"

	// Rejeições corrigíveis: o provedor enviou dados inconsistentes. A operação
	// fica registrada como REJECTED (replay devolve o mesmo resultado); para
	// corrigir, envia-se uma nova operação com outro externalTransactionId.
	FailureWalletPlayerMismatch    FailureCode = "WALLET_PLAYER_MISMATCH"
	FailureCurrencyMismatch        FailureCode = "CURRENCY_MISMATCH"
	FailureReferenceMismatch       FailureCode = "REFERENCE_MISMATCH"
	FailureReferenceAmountMismatch FailureCode = "REFERENCE_AMOUNT_MISMATCH"
	FailureInvalidReferenceKind    FailureCode = "INVALID_REFERENCE_KIND"

	// Falha permanente de processamento (FAILED), registrada para auditoria.
	FailureProcessingFailed FailureCode = "PROCESSING_FAILED"
)

var rejectionCodes = map[FailureCode]bool{
	FailureInsufficientFunds:            false,
	FailureInsufficientFundsForReversal: false,
	FailureReferenceNotFound:            false,
	FailureReferenceNotProcessed:        false,
	FailureAlreadyReversed:              false,
	FailureWalletPlayerMismatch:         true,
	FailureCurrencyMismatch:             true,
	FailureReferenceMismatch:            true,
	FailureReferenceAmountMismatch:      true,
	FailureInvalidReferenceKind:         true,
}

// IsRejection informa se o código é de rejeição de negócio (status REJECTED).
func (c FailureCode) IsRejection() bool {
	_, ok := rejectionCodes[c]
	return ok
}

// IsFailure informa se o código é de falha permanente (status FAILED).
func (c FailureCode) IsFailure() bool { return c == FailureProcessingFailed }

// IsCorrectable informa se a rejeição decorre de dados inconsistentes que o
// provedor pode corrigir em uma nova operação.
func (c FailureCode) IsCorrectable() bool { return rejectionCodes[c] }
