package domain

import (
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// Kind é o tipo da operação.
type Kind string

const (
	KindOpening  Kind = "OPENING" // interno: crédito inicial da carteira
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

// ParseKind valida um tipo vindo da persistência (aceita OPENING).
func ParseKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return k, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidKind, s)
	}
}

// ParseExternalKind valida um tipo recebido por HTTP ou SQS. OPENING é
// reservado à abertura interna e é rejeitado.
func ParseExternalKind(s string) (Kind, error) {
	k, err := ParseKind(s)
	if err != nil {
		return "", err
	}
	if k == KindOpening {
		return "", ErrOpeningNotAllowed
	}
	return k, nil
}

// IsReversal informa se o tipo desfaz outra transação.
func (k Kind) IsReversal() bool { return k == KindRefund || k == KindRollback }

// Status é o estado da transação.
type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED"
	StatusRejected         Status = "REJECTED"
	StatusFailed           Status = "FAILED"
)

// ParseStatus valida um estado vindo da persistência.
func ParseStatus(s string) (Status, error) {
	switch st := Status(s); st {
	case StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed:
		return st, nil
	default:
		return "", fmt.Errorf("%w: status %q", ErrInvalidTransaction, s)
	}
}

// IsTerminal informa se o estado é final.
func (s Status) IsTerminal() bool {
	return s == StatusProcessed || s == StatusRejected || s == StatusFailed
}

const maxExternalIDLength = 255

var payloadHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// WagerTransaction é uma operação financeira. Operações externas vêm de
// provedores (BET, WIN, LOSS, REFUND, ROLLBACK); OPENING é interna.
type WagerTransaction struct {
	id       uuid.UUID
	kind     Kind
	status   Status
	walletID uuid.UUID
	playerID uuid.UUID
	money    Money

	// Metadados externos (vazios em OPENING).
	providerID                     string
	externalTransactionID          string
	idempotencyKey                 string
	payloadHash                    string
	roundID                        string
	gameID                         string
	referenceExternalTransactionID string
	referenceTransactionID         uuid.UUID

	// Resultado.
	failureCode         FailureCode
	resultBalance       Money
	resultWalletVersion int64

	// Retomada de PENDING_REFERENCE.
	attempts      int
	nextAttemptAt time.Time

	createdAt   time.Time
	updatedAt   time.Time
	processedAt time.Time
}

// ExternalTransactionParams são os campos de uma operação recebida de um provedor.
type ExternalTransactionParams struct {
	ID                             uuid.UUID
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	PlayerID                       uuid.UUID
	WalletID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Money                          Money
	ReferenceExternalTransactionID string
	Now                            time.Time
}

// NewExternalTransaction valida uma operação de provedor e a cria em PENDING.
// Erros aqui são de entrada (corrigíveis pelo cliente) e a operação não é persistida.
func NewExternalTransaction(p ExternalTransactionParams) (*WagerTransaction, error) {
	if p.Kind == KindOpening {
		return nil, ErrOpeningNotAllowed
	}
	if _, err := ParseKind(string(p.Kind)); err != nil {
		return nil, err
	}
	if err := validateExternalFields(p); err != nil {
		return nil, err
	}
	if err := validateAmountPolicy(p.Kind, p.Money); err != nil {
		return nil, err
	}
	if err := validateReferencePolicy(p.Kind, p.ReferenceExternalTransactionID); err != nil {
		return nil, err
	}
	if p.Now.IsZero() {
		return nil, fmt.Errorf("%w: instante de criação vazio", ErrInvalidTransaction)
	}

	now := p.Now.UTC()
	return &WagerTransaction{
		id:                             p.ID,
		kind:                           p.Kind,
		status:                         StatusPending,
		walletID:                       p.WalletID,
		playerID:                       p.PlayerID,
		money:                          p.Money,
		providerID:                     p.ProviderID,
		externalTransactionID:          p.ExternalTransactionID,
		idempotencyKey:                 p.IdempotencyKey,
		payloadHash:                    p.PayloadHash,
		roundID:                        p.RoundID,
		gameID:                         p.GameID,
		referenceExternalTransactionID: p.ReferenceExternalTransactionID,
		createdAt:                      now,
		updatedAt:                      now,
	}, nil
}

func validateExternalFields(p ExternalTransactionParams) error {
	if p.ID == uuid.Nil || p.WalletID == uuid.Nil || p.PlayerID == uuid.Nil {
		return fmt.Errorf("%w: id, walletId e playerId são obrigatórios", ErrInvalidTransaction)
	}
	fields := []struct{ name, value string }{
		{"providerId", p.ProviderID},
		{"externalTransactionId", p.ExternalTransactionID},
		{"idempotencyKey", p.IdempotencyKey},
		{"roundId", p.RoundID},
		{"gameId", p.GameID},
	}
	for _, f := range fields {
		if f.value == "" {
			return fmt.Errorf("%w: %s é obrigatório", ErrInvalidTransaction, f.name)
		}
		if len(f.value) > maxExternalIDLength {
			return fmt.Errorf("%w: %s excede %d caracteres", ErrInvalidTransaction, f.name, maxExternalIDLength)
		}
	}
	if len(p.ReferenceExternalTransactionID) > maxExternalIDLength {
		return fmt.Errorf("%w: referenceExternalTransactionId excede %d caracteres", ErrInvalidTransaction, maxExternalIDLength)
	}
	if !payloadHashPattern.MatchString(p.PayloadHash) {
		return fmt.Errorf("%w: payloadHash deve ser SHA-256 em hex", ErrInvalidTransaction)
	}
	if !p.Money.IsValid() {
		return fmt.Errorf("%w: money", ErrUninitialized)
	}
	return nil
}

// validateAmountPolicy: LOSS exige exatamente zero; os demais exigem valor positivo.
func validateAmountPolicy(kind Kind, m Money) error {
	if m.IsNegative() {
		return fmt.Errorf("%w: %s", ErrNegativeAmount, m)
	}
	if kind == KindLoss {
		if !m.IsZero() {
			return fmt.Errorf("%w: LOSS exige amount 0.00, recebido %s", ErrInvalidAmount, m.Amount())
		}
		return nil
	}
	if !m.IsPositive() {
		return fmt.Errorf("%w: %s exige valor maior que zero", ErrNonPositiveAmount, kind)
	}
	return nil
}

// validateReferencePolicy: obrigatória em REFUND/ROLLBACK, opcional em WIN,
// proibida em BET, LOSS e OPENING.
func validateReferencePolicy(kind Kind, ref string) error {
	switch kind {
	case KindRefund, KindRollback:
		if ref == "" {
			return fmt.Errorf("%w: %s exige referenceExternalTransactionId", ErrInvalidTransaction, kind)
		}
	case KindWin:
	default:
		if ref != "" {
			return fmt.Errorf("%w: %s não aceita referenceExternalTransactionId", ErrInvalidTransaction, kind)
		}
	}
	return nil
}

// newOpeningTransaction cria o crédito inicial de uma carteira recém-aberta, já
// PROCESSED. Só é usado por OpenWallet.
func newOpeningTransaction(id uuid.UUID, w *Wallet) (*WagerTransaction, error) {
	if id == uuid.Nil {
		return nil, fmt.Errorf("%w: id vazio", ErrInvalidTransaction)
	}
	if w.Version() != InitialWalletVersion || !w.Balance().IsPositive() {
		return nil, fmt.Errorf("%w: OPENING exige carteira nova com saldo positivo", ErrInvalidTransaction)
	}
	return &WagerTransaction{
		id:                  id,
		kind:                KindOpening,
		status:              StatusProcessed,
		walletID:            w.ID(),
		playerID:            w.PlayerID(),
		money:               w.Balance(),
		resultBalance:       w.Balance(),
		resultWalletVersion: w.Version(),
		createdAt:           w.CreatedAt(),
		updatedAt:           w.CreatedAt(),
		processedAt:         w.CreatedAt(),
	}, nil
}

// TransactionSnapshot contém o estado persistido de uma transação.
type TransactionSnapshot struct {
	ID                             uuid.UUID
	Kind                           Kind
	Status                         Status
	WalletID                       uuid.UUID
	PlayerID                       uuid.UUID
	Money                          Money
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	RoundID                        string
	GameID                         string
	ReferenceExternalTransactionID string
	ReferenceTransactionID         uuid.UUID
	FailureCode                    FailureCode
	ResultBalance                  Money // zero-value quando ausente
	ResultWalletVersion            int64 // 0 quando ausente
	Attempts                       int
	NextAttemptAt                  time.Time
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
	ProcessedAt                    time.Time
}

// RehydrateTransaction reconstrói uma transação lida do banco, validando a
// coerência do estado sem reaplicar transições nem gerar eventos.
func RehydrateTransaction(s TransactionSnapshot) (*WagerTransaction, error) {
	if _, err := ParseKind(string(s.Kind)); err != nil {
		return nil, err
	}
	if _, err := ParseStatus(string(s.Status)); err != nil {
		return nil, err
	}
	if s.ID == uuid.Nil || s.WalletID == uuid.Nil || s.PlayerID == uuid.Nil {
		return nil, fmt.Errorf("%w: identificadores vazios", ErrInvalidTransaction)
	}
	if s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() {
		return nil, fmt.Errorf("%w: timestamps vazios", ErrInvalidTransaction)
	}
	if !s.Money.IsValid() {
		return nil, fmt.Errorf("%w: money", ErrUninitialized)
	}

	if s.Kind == KindOpening {
		if s.ProviderID != "" || s.ExternalTransactionID != "" || s.IdempotencyKey != "" || s.PayloadHash != "" ||
			s.RoundID != "" || s.GameID != "" || s.ReferenceExternalTransactionID != "" {
			return nil, fmt.Errorf("%w: OPENING não tem metadados externos", ErrInvalidTransaction)
		}
		if !s.Money.IsPositive() {
			return nil, fmt.Errorf("%w: OPENING exige valor positivo", ErrInvalidTransaction)
		}
	} else {
		if err := validateExternalFields(ExternalTransactionParams{
			ID: s.ID, ProviderID: s.ProviderID, ExternalTransactionID: s.ExternalTransactionID,
			IdempotencyKey: s.IdempotencyKey, PayloadHash: s.PayloadHash, PlayerID: s.PlayerID,
			WalletID: s.WalletID, RoundID: s.RoundID, GameID: s.GameID, Money: s.Money,
			ReferenceExternalTransactionID: s.ReferenceExternalTransactionID,
		}); err != nil {
			return nil, err
		}
		if err := validateAmountPolicy(s.Kind, s.Money); err != nil {
			return nil, err
		}
		if err := validateReferencePolicy(s.Kind, s.ReferenceExternalTransactionID); err != nil {
			return nil, err
		}
	}

	switch s.Status {
	case StatusProcessed:
		if s.FailureCode != "" || !s.ResultBalance.IsValid() || s.ResultWalletVersion < 1 || s.ProcessedAt.IsZero() {
			return nil, fmt.Errorf("%w: PROCESSED sem resultado completo", ErrInvalidTransaction)
		}
	case StatusRejected, StatusFailed:
		if s.FailureCode == "" || s.ProcessedAt.IsZero() {
			return nil, fmt.Errorf("%w: %s exige failureCode e processedAt", ErrInvalidTransaction, s.Status)
		}
	case StatusPendingReference:
		if s.ReferenceExternalTransactionID == "" || s.NextAttemptAt.IsZero() {
			return nil, fmt.Errorf("%w: PENDING_REFERENCE exige referência e próxima tentativa", ErrInvalidTransaction)
		}
	}

	return &WagerTransaction{
		id:                             s.ID,
		kind:                           s.Kind,
		status:                         s.Status,
		walletID:                       s.WalletID,
		playerID:                       s.PlayerID,
		money:                          s.Money,
		providerID:                     s.ProviderID,
		externalTransactionID:          s.ExternalTransactionID,
		idempotencyKey:                 s.IdempotencyKey,
		payloadHash:                    s.PayloadHash,
		roundID:                        s.RoundID,
		gameID:                         s.GameID,
		referenceExternalTransactionID: s.ReferenceExternalTransactionID,
		referenceTransactionID:         s.ReferenceTransactionID,
		failureCode:                    s.FailureCode,
		resultBalance:                  s.ResultBalance,
		resultWalletVersion:            s.ResultWalletVersion,
		attempts:                       s.Attempts,
		nextAttemptAt:                  s.NextAttemptAt.UTC(),
		createdAt:                      s.CreatedAt.UTC(),
		updatedAt:                      s.UpdatedAt.UTC(),
		processedAt:                    s.ProcessedAt.UTC(),
	}, nil
}

// Snapshot devolve o estado atual para persistência.
func (t *WagerTransaction) Snapshot() TransactionSnapshot {
	return TransactionSnapshot{
		ID:                             t.id,
		Kind:                           t.kind,
		Status:                         t.status,
		WalletID:                       t.walletID,
		PlayerID:                       t.playerID,
		Money:                          t.money,
		ProviderID:                     t.providerID,
		ExternalTransactionID:          t.externalTransactionID,
		IdempotencyKey:                 t.idempotencyKey,
		PayloadHash:                    t.payloadHash,
		RoundID:                        t.roundID,
		GameID:                         t.gameID,
		ReferenceExternalTransactionID: t.referenceExternalTransactionID,
		ReferenceTransactionID:         t.referenceTransactionID,
		FailureCode:                    t.failureCode,
		ResultBalance:                  t.resultBalance,
		ResultWalletVersion:            t.resultWalletVersion,
		Attempts:                       t.attempts,
		NextAttemptAt:                  t.nextAttemptAt,
		CreatedAt:                      t.createdAt,
		UpdatedAt:                      t.updatedAt,
		ProcessedAt:                    t.processedAt,
	}
}

func (t *WagerTransaction) ID() uuid.UUID                     { return t.id }
func (t *WagerTransaction) Kind() Kind                        { return t.kind }
func (t *WagerTransaction) Status() Status                    { return t.status }
func (t *WagerTransaction) WalletID() uuid.UUID               { return t.walletID }
func (t *WagerTransaction) PlayerID() uuid.UUID               { return t.playerID }
func (t *WagerTransaction) Money() Money                      { return t.money }
func (t *WagerTransaction) ProviderID() string                { return t.providerID }
func (t *WagerTransaction) ExternalTransactionID() string     { return t.externalTransactionID }
func (t *WagerTransaction) IdempotencyKey() string            { return t.idempotencyKey }
func (t *WagerTransaction) PayloadHash() string               { return t.payloadHash }
func (t *WagerTransaction) RoundID() string                   { return t.roundID }
func (t *WagerTransaction) GameID() string                    { return t.gameID }
func (t *WagerTransaction) ReferenceTransactionID() uuid.UUID { return t.referenceTransactionID }
func (t *WagerTransaction) FailureCode() FailureCode          { return t.failureCode }
func (t *WagerTransaction) Attempts() int                     { return t.attempts }
func (t *WagerTransaction) NextAttemptAt() time.Time          { return t.nextAttemptAt }
func (t *WagerTransaction) CreatedAt() time.Time              { return t.createdAt }
func (t *WagerTransaction) UpdatedAt() time.Time              { return t.updatedAt }
func (t *WagerTransaction) ProcessedAt() time.Time            { return t.processedAt }

func (t *WagerTransaction) ReferenceExternalTransactionID() string {
	return t.referenceExternalTransactionID
}

// IsExternal informa se a transação veio de um provedor.
func (t *WagerTransaction) IsExternal() bool { return t.kind != KindOpening }

// HasReference informa se a operação aponta para outra transação.
func (t *WagerTransaction) HasReference() bool { return t.referenceExternalTransactionID != "" }

// ResultBalance devolve o saldo observado no processamento, se houver.
func (t *WagerTransaction) ResultBalance() (Money, bool) {
	return t.resultBalance, t.resultBalance.IsValid()
}

// ResultWalletVersion devolve a versão da carteira observada no processamento (0 se ausente).
func (t *WagerTransaction) ResultWalletVersion() int64 { return t.resultWalletVersion }

// --- Transições de estado ---

func (t *WagerTransaction) checkOpen(to Status) error {
	if t.status.IsTerminal() {
		return fmt.Errorf("%w: %s -> %s (transação %s é terminal)", ErrInvalidTransition, t.status, to, t.id)
	}
	return nil
}

func (t *WagerTransaction) touch(now time.Time) time.Time {
	now = now.UTC()
	if now.Before(t.updatedAt) {
		now = t.updatedAt
	}
	t.updatedAt = now
	return now
}

// markProcessed conclui a operação com o saldo e a versão observados.
func (t *WagerTransaction) markProcessed(balance Money, walletVersion int64, now time.Time) error {
	if err := t.checkOpen(StatusProcessed); err != nil {
		return err
	}
	if !balance.IsValid() || balance.IsNegative() || walletVersion < 1 {
		return fmt.Errorf("%w: resultado inválido", ErrInvalidTransaction)
	}
	t.status = StatusProcessed
	t.resultBalance = balance
	t.resultWalletVersion = walletVersion
	t.nextAttemptAt = time.Time{}
	t.processedAt = t.touch(now)
	return nil
}

// markRejected encerra a operação por regra de negócio. O saldo observado é
// opcional (zero-value quando a carteira não pôde ser avaliada).
func (t *WagerTransaction) markRejected(code FailureCode, balance Money, walletVersion int64, now time.Time) error {
	if err := t.checkOpen(StatusRejected); err != nil {
		return err
	}
	if !code.IsRejection() {
		return fmt.Errorf("%w: código de rejeição inválido %q", ErrInvalidTransaction, code)
	}
	t.status = StatusRejected
	t.failureCode = code
	t.resultBalance = balance
	t.resultWalletVersion = walletVersion
	t.nextAttemptAt = time.Time{}
	t.processedAt = t.touch(now)
	return nil
}

// MarkFailed registra uma falha permanente de infraestrutura, para auditoria.
func (t *WagerTransaction) MarkFailed(code FailureCode, now time.Time) error {
	if err := t.checkOpen(StatusFailed); err != nil {
		return err
	}
	if !code.IsFailure() {
		return fmt.Errorf("%w: código de falha inválido %q", ErrInvalidTransaction, code)
	}
	t.status = StatusFailed
	t.failureCode = code
	t.nextAttemptAt = time.Time{}
	t.processedAt = t.touch(now)
	return nil
}

// awaitReference coloca (ou mantém) a operação em PENDING_REFERENCE e agenda a
// próxima tentativa conforme a política. Esgotadas as tentativas, rejeita com
// REFERENCE_NOT_FOUND e devolve false.
func (t *WagerTransaction) awaitReference(policy ReferenceRetryPolicy, now time.Time) (waiting bool, err error) {
	if err := t.checkOpen(StatusPendingReference); err != nil {
		return false, err
	}
	if !t.HasReference() {
		return false, fmt.Errorf("%w: %s sem referência não pode aguardar referência", ErrInvalidTransition, t.kind)
	}
	if t.attempts >= policy.MaxAttempts {
		return false, t.markRejected(FailureReferenceNotFound, Money{}, 0, now)
	}
	now = t.touch(now)
	t.status = StatusPendingReference
	t.nextAttemptAt = now.Add(policy.Delay(t.attempts))
	t.attempts++
	return true, nil
}

// resolveReference grava a transação interna referenciada. Só pode ser definida uma vez.
func (t *WagerTransaction) resolveReference(ref *WagerTransaction) error {
	if t.referenceTransactionID != uuid.Nil && t.referenceTransactionID != ref.id {
		return fmt.Errorf("%w: referência já resolvida", ErrInvalidTransition)
	}
	t.referenceTransactionID = ref.id
	return nil
}

// ReferenceRetryPolicy define o backoff exponencial de PENDING_REFERENCE.
type ReferenceRetryPolicy struct {
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxAttempts int
}

// DefaultReferenceRetryPolicy: 1s, 2s, 4s ... limitado a 5min, 12 tentativas
// (cerca de 16 minutos no total).
var DefaultReferenceRetryPolicy = ReferenceRetryPolicy{
	BaseDelay:   time.Second,
	MaxDelay:    5 * time.Minute,
	MaxAttempts: 12,
}

// Delay devolve a espera antes da tentativa de número attempt (começando em 0).
func (p ReferenceRetryPolicy) Delay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 62 {
		return p.MaxDelay
	}
	factor := int64(1) << attempt
	if p.BaseDelay > 0 && factor > int64(math.MaxInt64/p.BaseDelay) {
		return p.MaxDelay
	}
	d := p.BaseDelay * time.Duration(factor)
	if d > p.MaxDelay {
		return p.MaxDelay
	}
	return d
}
