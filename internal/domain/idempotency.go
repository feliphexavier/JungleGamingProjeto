package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// PayloadFields são os campos de negócio de uma operação externa: exatamente
// o que entra no hash de idempotência. HTTP e SQS montam esta mesma estrutura,
// então a mesma operação tem o mesmo hash pelos dois canais.
//
// Ficam de fora: a própria chave de idempotência e metadados de transporte
// (headers, messageId, occurredAt, type do envelope SQS).
type PayloadFields struct {
	ProviderID                     string
	ExternalTransactionID          string
	PlayerID                       uuid.UUID
	WalletID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Money                          Money
	ReferenceExternalTransactionID string
}

// PayloadHash calcula o SHA-256 (hex minúsculo) do JSON canônico dos campos de
// negócio. JSON canônico aqui significa: objeto com chaves em ordem
// lexicográfica, sem espaços, sem escape de HTML, UUIDs no formato canônico
// minúsculo, money com amount em duas casas, e referenceExternalTransactionId
// omitido quando vazio.
func PayloadHash(f PayloadFields) (string, error) {
	if !f.Money.IsValid() {
		return "", fmt.Errorf("%w: money", ErrUninitialized)
	}
	canonical := map[string]any{
		"externalTransactionId": f.ExternalTransactionID,
		"gameId":                f.GameID,
		"kind":                  string(f.Kind),
		"money": map[string]string{
			"amount":   f.Money.Amount(),
			"currency": f.Money.Currency().String(),
		},
		"playerId":   f.PlayerID.String(),
		"providerId": f.ProviderID,
		"roundId":    f.RoundID,
		"walletId":   f.WalletID.String(),
	}
	if f.ReferenceExternalTransactionID != "" {
		canonical["referenceExternalTransactionId"] = f.ReferenceExternalTransactionID
	}

	data, err := canonicalJSON(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalJSON serializa com chaves ordenadas (encoding/json ordena as chaves
// de maps), sem espaços e sem escape de HTML.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
