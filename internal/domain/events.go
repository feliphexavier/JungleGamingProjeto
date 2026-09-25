package domain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Tipos de evento de integração.
const (
	EventWagerTransactionProcessed        = "WagerTransactionProcessed"
	EventWagerTransactionRejected         = "WagerTransactionRejected"
	EventWalletBalanceChanged             = "WalletBalanceChanged"
	EventWagerTransactionPendingReference = "WagerTransactionPendingReference"
)

// Tipos de agregado.
const (
	AggregateWagerTransaction = "WagerTransaction"
	AggregateWallet           = "Wallet"
)

// EventData é o payload tipado de um evento. Cada tipo concreto define seu
// eventType e sua versão, que não podem ser escolhidos por quem cria o evento.
type EventData interface {
	EventType() string
	EventVersion() int
}

// WagerTransactionProcessedData: operação concluída com sucesso, inclusive LOSS
// e OPENING. Campos externos são omitidos no OPENING.
type WagerTransactionProcessedData struct {
	TransactionID          uuid.UUID  `json:"transactionId"`
	WalletID               uuid.UUID  `json:"walletId"`
	PlayerID               uuid.UUID  `json:"playerId"`
	Kind                   Kind       `json:"kind"`
	Money                  Money      `json:"money"`
	Balance                Money      `json:"balance"`
	WalletVersion          int64      `json:"walletVersion"`
	ProviderID             string     `json:"providerId,omitempty"`
	ExternalTransactionID  string     `json:"externalTransactionId,omitempty"`
	RoundID                string     `json:"roundId,omitempty"`
	GameID                 string     `json:"gameId,omitempty"`
	ReferenceTransactionID *uuid.UUID `json:"referenceTransactionId,omitempty"`
	ProcessedAt            time.Time  `json:"processedAt"`
}

func (WagerTransactionProcessedData) EventType() string { return EventWagerTransactionProcessed }
func (WagerTransactionProcessedData) EventVersion() int { return 1 }

// WagerTransactionRejectedData: rejeição definitiva por regra de negócio.
type WagerTransactionRejectedData struct {
	TransactionID         uuid.UUID   `json:"transactionId"`
	WalletID              uuid.UUID   `json:"walletId"`
	PlayerID              uuid.UUID   `json:"playerId"`
	ProviderID            string      `json:"providerId"`
	ExternalTransactionID string      `json:"externalTransactionId"`
	Kind                  Kind        `json:"kind"`
	Money                 Money       `json:"money"`
	FailureCode           FailureCode `json:"failureCode"`
	RejectedAt            time.Time   `json:"rejectedAt"`
}

func (WagerTransactionRejectedData) EventType() string { return EventWagerTransactionRejected }
func (WagerTransactionRejectedData) EventVersion() int { return 1 }

// WalletBalanceChangedData: alteração efetiva do saldo.
type WalletBalanceChangedData struct {
	WalletID      uuid.UUID `json:"walletId"`
	TransactionID uuid.UUID `json:"transactionId"`
	Direction     Direction `json:"direction"`
	Money         Money     `json:"money"`
	BalanceBefore Money     `json:"balanceBefore"`
	BalanceAfter  Money     `json:"balanceAfter"`
	WalletVersion int64     `json:"walletVersion"`
}

func (WalletBalanceChangedData) EventType() string { return EventWalletBalanceChanged }
func (WalletBalanceChangedData) EventVersion() int { return 1 }

// WagerTransactionPendingReferenceData: a operação aguarda sua referência.
type WagerTransactionPendingReferenceData struct {
	TransactionID                  uuid.UUID `json:"transactionId"`
	WalletID                       uuid.UUID `json:"walletId"`
	ProviderID                     string    `json:"providerId"`
	ExternalTransactionID          string    `json:"externalTransactionId"`
	Kind                           Kind      `json:"kind"`
	ReferenceExternalTransactionID string    `json:"referenceExternalTransactionId"`
	NextAttemptAt                  time.Time `json:"nextAttemptAt"`
}

func (WagerTransactionPendingReferenceData) EventType() string {
	return EventWagerTransactionPendingReference
}
func (WagerTransactionPendingReferenceData) EventVersion() int { return 1 }

// Event é um evento de integração imutável. É gravado na outbox na mesma
// transação SQL da mudança que o originou e publicado depois do commit;
// republicações preservam o EventID.
type Event struct {
	id            uuid.UUID
	aggregateType string
	aggregateID   uuid.UUID
	walletID      uuid.UUID
	correlationID string
	causationID   string
	occurredAt    time.Time
	data          EventData
}

// EventMeta são os identificadores de rastreio propagados para os eventos.
type EventMeta struct {
	CorrelationID string
	CausationID   string // opcional
}

func newEvent(id uuid.UUID, aggregateType string, aggregateID, walletID uuid.UUID, meta EventMeta, occurredAt time.Time, data EventData) (Event, error) {
	if id == uuid.Nil || aggregateID == uuid.Nil || walletID == uuid.Nil {
		return Event{}, fmt.Errorf("%w: evento sem identificadores", ErrInvalidTransaction)
	}
	if meta.CorrelationID == "" {
		return Event{}, fmt.Errorf("%w: correlationId obrigatório", ErrInvalidTransaction)
	}
	if occurredAt.IsZero() {
		return Event{}, fmt.Errorf("%w: occurredAt vazio", ErrInvalidTransaction)
	}
	return Event{
		id:            id,
		aggregateType: aggregateType,
		aggregateID:   aggregateID,
		walletID:      walletID,
		correlationID: meta.CorrelationID,
		causationID:   meta.CausationID,
		occurredAt:    occurredAt.UTC(),
		data:          data,
	}, nil
}

func (e Event) ID() uuid.UUID          { return e.id }
func (e Event) Type() string           { return e.data.EventType() }
func (e Event) Version() int           { return e.data.EventVersion() }
func (e Event) AggregateType() string  { return e.aggregateType }
func (e Event) AggregateID() uuid.UUID { return e.aggregateID }
func (e Event) CorrelationID() string  { return e.correlationID }
func (e Event) CausationID() string    { return e.causationID }
func (e Event) OccurredAt() time.Time  { return e.occurredAt }
func (e Event) Data() EventData        { return e.data }

// WalletID é a carteira afetada; usada como chave de ordenação na publicação
// (MessageGroupId), para manter a ordem por carteira.
func (e Event) WalletID() uuid.UUID { return e.walletID }

type eventEnvelope struct {
	EventID       uuid.UUID `json:"eventId"`
	EventType     string    `json:"eventType"`
	AggregateID   uuid.UUID `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   string    `json:"causationId,omitempty"`
	OccurredAt    string    `json:"occurredAt"`
	Version       int       `json:"version"`
	Data          EventData `json:"data"`
}

// MarshalJSON produz o envelope publicado; o resultado é o snapshot imutável
// gravado na outbox. Timestamps em UTC, RFC 3339; valores monetários em
// strings decimais.
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(eventEnvelope{
		EventID:       e.id,
		EventType:     e.data.EventType(),
		AggregateID:   e.aggregateID,
		CorrelationID: e.correlationID,
		CausationID:   e.causationID,
		OccurredAt:    e.occurredAt.Format(time.RFC3339Nano),
		Version:       e.data.EventVersion(),
		Data:          e.data,
	})
}

// TransactionEvents devolve os eventos correspondentes ao estado atual da
// transação, após ProcessWager ou OpenWallet:
//
//   - PROCESSED: WagerTransactionProcessed e, se houve lançamento, WalletBalanceChanged
//   - REJECTED: WagerTransactionRejected
//   - PENDING_REFERENCE: WagerTransactionPendingReference, só na primeira espera
//
// newID gera os eventIds.
func TransactionEvents(tx *WagerTransaction, entry *LedgerEntry, meta EventMeta, newID func() uuid.UUID) ([]Event, error) {
	switch tx.Status() {
	case StatusProcessed:
		return processedEvents(tx, entry, meta, newID)
	case StatusRejected:
		ev, err := newEvent(newID(), AggregateWagerTransaction, tx.ID(), tx.WalletID(), meta, tx.ProcessedAt(),
			WagerTransactionRejectedData{
				TransactionID:         tx.ID(),
				WalletID:              tx.WalletID(),
				PlayerID:              tx.PlayerID(),
				ProviderID:            tx.ProviderID(),
				ExternalTransactionID: tx.ExternalTransactionID(),
				Kind:                  tx.Kind(),
				Money:                 tx.Money(),
				FailureCode:           tx.FailureCode(),
				RejectedAt:            tx.ProcessedAt(),
			})
		if err != nil {
			return nil, err
		}
		return []Event{ev}, nil
	case StatusPendingReference:
		if tx.Attempts() != 1 {
			return nil, nil // já anunciado na primeira espera
		}
		ev, err := newEvent(newID(), AggregateWagerTransaction, tx.ID(), tx.WalletID(), meta, tx.UpdatedAt(),
			WagerTransactionPendingReferenceData{
				TransactionID:                  tx.ID(),
				WalletID:                       tx.WalletID(),
				ProviderID:                     tx.ProviderID(),
				ExternalTransactionID:          tx.ExternalTransactionID(),
				Kind:                           tx.Kind(),
				ReferenceExternalTransactionID: tx.ReferenceExternalTransactionID(),
				NextAttemptAt:                  tx.NextAttemptAt(),
			})
		if err != nil {
			return nil, err
		}
		return []Event{ev}, nil
	default:
		return nil, nil
	}
}

func processedEvents(tx *WagerTransaction, entry *LedgerEntry, meta EventMeta, newID func() uuid.UUID) ([]Event, error) {
	balance, _ := tx.ResultBalance()
	data := WagerTransactionProcessedData{
		TransactionID:         tx.ID(),
		WalletID:              tx.WalletID(),
		PlayerID:              tx.PlayerID(),
		Kind:                  tx.Kind(),
		Money:                 tx.Money(),
		Balance:               balance,
		WalletVersion:         tx.ResultWalletVersion(),
		ProviderID:            tx.ProviderID(),
		ExternalTransactionID: tx.ExternalTransactionID(),
		RoundID:               tx.RoundID(),
		GameID:                tx.GameID(),
		ProcessedAt:           tx.ProcessedAt(),
	}
	if ref := tx.ReferenceTransactionID(); ref != uuid.Nil {
		data.ReferenceTransactionID = &ref
	}
	processed, err := newEvent(newID(), AggregateWagerTransaction, tx.ID(), tx.WalletID(), meta, tx.ProcessedAt(), data)
	if err != nil {
		return nil, err
	}
	events := []Event{processed}

	if entry != nil {
		if entry.TransactionID() != tx.ID() {
			return nil, fmt.Errorf("%w: lançamento de outra transação", ErrInvalidLedgerEntry)
		}
		changed, err := newEvent(newID(), AggregateWallet, entry.WalletID(), entry.WalletID(), meta, entry.CreatedAt(),
			WalletBalanceChangedData{
				WalletID:      entry.WalletID(),
				TransactionID: entry.TransactionID(),
				Direction:     entry.Direction(),
				Money:         entry.Amount(),
				BalanceBefore: entry.BalanceBefore(),
				BalanceAfter:  entry.BalanceAfter(),
				WalletVersion: entry.WalletVersion(),
			})
		if err != nil {
			return nil, err
		}
		events = append(events, changed)
	}
	return events, nil
}
