package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
)

const maxBodyBytes = 64 << 10

// moneyDTO recebe amount como string. Um número JSON (25.00 sem aspas) falha na
// decodificação para string, então nunca passa por float.
type moneyDTO struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type openWalletRequest struct {
	PlayerID       string   `json:"playerId"`
	InitialBalance moneyDTO `json:"initialBalance"`
}

type wagerRequest struct {
	ProviderID                     string   `json:"providerId"`
	ExternalTransactionID          string   `json:"externalTransactionId"`
	PlayerID                       string   `json:"playerId"`
	WalletID                       string   `json:"walletId"`
	RoundID                        string   `json:"roundId"`
	GameID                         string   `json:"gameId"`
	Kind                           string   `json:"kind"`
	Money                          moneyDTO `json:"money"`
	ReferenceExternalTransactionID string   `json:"referenceExternalTransactionId,omitempty"`
}

type walletResponse struct {
	ID       uuid.UUID    `json:"id"`
	PlayerID uuid.UUID    `json:"playerId"`
	Balance  domain.Money `json:"balance"`
	Version  int64        `json:"version"`
}

func toWallet(w *domain.Wallet) walletResponse {
	return walletResponse{ID: w.ID(), PlayerID: w.PlayerID(), Balance: w.Balance(), Version: w.Version()}
}

// wagerResponse é a resposta de POST /wagering/transactions.
type wagerResponse struct {
	TransactionID    uuid.UUID     `json:"transactionId"`
	Status           domain.Status `json:"status"`
	Balance          *domain.Money `json:"balance,omitempty"`
	FailureCode      string        `json:"failureCode,omitempty"`
	IdempotentReplay bool          `json:"idempotentReplay"`
}

func toWagerResponse(r app.SubmitWagerResult) wagerResponse {
	tx := r.Transaction
	resp := wagerResponse{
		TransactionID:    tx.ID(),
		Status:           tx.Status(),
		FailureCode:      string(tx.FailureCode()),
		IdempotentReplay: r.Replay,
	}
	if bal, ok := tx.ResultBalance(); ok {
		resp.Balance = &bal
	}
	return resp
}

// transactionResponse é a visão completa usada nas consultas.
type transactionResponse struct {
	TransactionID                  uuid.UUID     `json:"transactionId"`
	Kind                           domain.Kind   `json:"kind"`
	Status                         domain.Status `json:"status"`
	WalletID                       uuid.UUID     `json:"walletId"`
	PlayerID                       uuid.UUID     `json:"playerId"`
	Money                          domain.Money  `json:"money"`
	ProviderID                     string        `json:"providerId,omitempty"`
	ExternalTransactionID          string        `json:"externalTransactionId,omitempty"`
	RoundID                        string        `json:"roundId,omitempty"`
	GameID                         string        `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string        `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         *uuid.UUID    `json:"referenceTransactionId,omitempty"`
	FailureCode                    string        `json:"failureCode,omitempty"`
	Balance                        *domain.Money `json:"balance,omitempty"`
	WalletVersion                  int64         `json:"walletVersion,omitempty"`
	Attempts                       int           `json:"attempts,omitempty"`
	NextAttemptAt                  *time.Time    `json:"nextAttemptAt,omitempty"`
	CreatedAt                      time.Time     `json:"createdAt"`
	UpdatedAt                      time.Time     `json:"updatedAt"`
	ProcessedAt                    *time.Time    `json:"processedAt,omitempty"`
}

func toTransaction(tx *domain.WagerTransaction) transactionResponse {
	resp := transactionResponse{
		TransactionID:                  tx.ID(),
		Kind:                           tx.Kind(),
		Status:                         tx.Status(),
		WalletID:                       tx.WalletID(),
		PlayerID:                       tx.PlayerID(),
		Money:                          tx.Money(),
		ProviderID:                     tx.ProviderID(),
		ExternalTransactionID:          tx.ExternalTransactionID(),
		RoundID:                        tx.RoundID(),
		GameID:                         tx.GameID(),
		ReferenceExternalTransactionID: tx.ReferenceExternalTransactionID(),
		FailureCode:                    string(tx.FailureCode()),
		WalletVersion:                  tx.ResultWalletVersion(),
		Attempts:                       tx.Attempts(),
		CreatedAt:                      tx.CreatedAt(),
		UpdatedAt:                      tx.UpdatedAt(),
	}
	if ref := tx.ReferenceTransactionID(); ref != uuid.Nil {
		resp.ReferenceTransactionID = &ref
	}
	if bal, ok := tx.ResultBalance(); ok {
		resp.Balance = &bal
	}
	if t := tx.NextAttemptAt(); !t.IsZero() {
		resp.NextAttemptAt = &t
	}
	if t := tx.ProcessedAt(); !t.IsZero() {
		resp.ProcessedAt = &t
	}
	return resp
}

type ledgerEntryResponse struct {
	ID            uuid.UUID        `json:"id"`
	TransactionID uuid.UUID        `json:"transactionId"`
	Direction     domain.Direction `json:"direction"`
	Money         domain.Money     `json:"money"`
	BalanceBefore domain.Money     `json:"balanceBefore"`
	BalanceAfter  domain.Money     `json:"balanceAfter"`
	WalletVersion int64            `json:"walletVersion"`
	CreatedAt     time.Time        `json:"createdAt"`
}

type ledgerResponse struct {
	WalletID   uuid.UUID             `json:"walletId"`
	Items      []ledgerEntryResponse `json:"items"`
	NextCursor string                `json:"nextCursor,omitempty"`
}

type reconciliationResponse struct {
	WalletID          uuid.UUID    `json:"walletId"`
	StoredBalance     domain.Money `json:"storedBalance"`
	CalculatedBalance domain.Money `json:"calculatedBalance"`
	Difference        domain.Money `json:"difference"`
	Consistent        bool         `json:"consistent"`
	CheckedEntries    int64        `json:"checkedEntries"`
}

// decodeJSON lê exatamente um objeto JSON, recusando campos desconhecidos e
// corpos acima de maxBodyBytes.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: corpo JSON inválido: %v", app.ErrInvalidInput, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: corpo deve conter um único objeto JSON", app.ErrInvalidInput)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func parseUUIDParam(value, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %s inválido", app.ErrInvalidInput, name)
	}
	return id, nil
}
