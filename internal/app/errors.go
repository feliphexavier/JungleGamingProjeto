package app

import "errors"

// Erros dos casos de uso, classificáveis com errors.Is. A camada HTTP e o
// consumidor SQS traduzem cada um em código de status ou decisão de retry.
var (
	// ErrInvalidInput: entrada malformada ou fora das regras de formato. Os
	// erros de domínio originais continuam encadeados (errors.Is funciona para ambos).
	ErrInvalidInput = errors.New("invalid input")

	// ErrForbidden: a identidade autenticada não pode executar a operação.
	ErrForbidden = errors.New("forbidden")

	// ErrIdempotencyConflict: chave reutilizada com outro conteúdo, ou a mesma
	// operação (providerId, externalTransactionId) enviada com outra chave.
	ErrIdempotencyConflict = errors.New("idempotency conflict")

	// ErrInboxConflict: mesmo messageId reentregue com conteúdo diferente.
	ErrInboxConflict = errors.New("inbox message conflict")

	ErrWalletNotFound      = errors.New("wallet not found")
	ErrWalletAlreadyExists = errors.New("wallet already exists")
	ErrTransactionNotFound = errors.New("transaction not found")

	// ErrDuplicate: violação de unicidade na gravação. Indica corrida com outra
	// instância; o caso de uso refaz a operação e passa a ver o registro vencedor.
	ErrDuplicate = errors.New("duplicate record")

	// ErrConcurrentUpdate: a carteira mudou entre a leitura e a escrita.
	ErrConcurrentUpdate = errors.New("concurrent wallet update")

	// ErrUnavailable: falha transitória de infraestrutura (banco fora, timeout).
	// Nada foi commitado; a operação pode ser repetida.
	ErrUnavailable = errors.New("temporarily unavailable")
)
