package sqs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
)

// ConsumerName identifica este consumidor na inbox.
const ConsumerName = "wager-operations"

// AuthorizationAttribute é o atributo da mensagem com o access token do
// provedor ("Bearer <jwt>"), obtido no IdP como no HTTP.
const AuthorizationAttribute = "Authorization"

// MessageType é o único tipo de mensagem aceito na fila de entrada.
const MessageType = "WagerOperationRequested"

// Authenticator valida o token do provedor (mesma validação do HTTP).
type Authenticator interface {
	Authenticate(ctx context.Context, rawToken string) (app.Principal, error)
}

// WagerSubmitter é o caso de uso compartilhado com o HTTP.
type WagerSubmitter interface {
	SubmitWager(ctx context.Context, cmd app.SubmitWagerCommand) (app.SubmitWagerResult, error)
}

// ConsumerConfig ajusta o consumo.
type ConsumerConfig struct {
	Queues *Queues
	// WaitTime é o long polling do ReceiveMessage (máximo 20s).
	WaitTime time.Duration
	// MaxMessages por recebimento (máximo 10).
	MaxMessages int32
	// ProcessTimeout é o prazo para processar uma mensagem; deve ser menor que
	// o visibility timeout da fila.
	ProcessTimeout time.Duration
}

// Consumer lê operações de provedores e as processa com o mesmo caso de uso
// do HTTP, com deduplicação durável pela inbox.
//
// Decisão por mensagem:
//   - processada (qualquer resultado terminal, inclusive REJECTED e
//     PENDING_REFERENCE) ou replay: a mensagem é apagada;
//   - falha permanente (envelope inválido, token inválido, sem permissão,
//     conflito de idempotência, carteira inexistente): enviada à DLQ com o
//     motivo, sem o token, e apagada;
//   - falha transitória (banco indisponível, disputa esgotada): a mensagem não
//     é apagada e volta após o visibility timeout; a redrive policy da fila é
//     a última barreira contra mensagens que nunca passam.
type Consumer struct {
	client *sqs.Client
	auth   Authenticator
	svc    WagerSubmitter
	cfg    ConsumerConfig
	log    *slog.Logger
}

func NewConsumer(client *sqs.Client, auth Authenticator, svc WagerSubmitter, cfg ConsumerConfig, log *slog.Logger) *Consumer {
	return &Consumer{client: client, auth: auth, svc: svc, cfg: cfg, log: log.With(slog.String("consumer", ConsumerName))}
}

// errorBackoff evita um laço apertado quando o SQS está indisponível.
const errorBackoff = 2 * time.Second

// Poll faz um recebimento (long polling) e processa as mensagens recebidas.
// Devolve quantas foram concluídas (apagadas).
func (c *Consumer) Poll(ctx context.Context) (int, error) {
	out, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:              aws.String(c.cfg.Queues.Inbound),
		MaxNumberOfMessages:   c.cfg.MaxMessages,
		WaitTimeSeconds:       int32(c.cfg.WaitTime / time.Second),
		MessageAttributeNames: []string{"All"},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{
			types.MessageSystemAttributeNameMessageGroupId,
			types.MessageSystemAttributeNameApproximateReceiveCount,
		},
	})
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		select {
		case <-ctx.Done():
		case <-time.After(errorBackoff):
		}
		return 0, fmt.Errorf("sqs: receber: %w", err)
	}

	// Mensagens do mesmo grupo chegam em ordem; se uma não for concluída, as
	// seguintes do grupo também não são processadas neste ciclo.
	var (
		done    int
		blocked = map[string]bool{}
		release []types.Message
	)
	for _, m := range out.Messages {
		group := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
		if ctx.Err() != nil || (group != "" && blocked[group]) {
			release = append(release, m)
			continue
		}
		if c.handle(ctx, m) {
			done++
		} else if group != "" {
			blocked[group] = true
		}
	}
	c.releaseNow(release)
	return done, nil
}

// handle processa uma mensagem e informa se ela foi concluída (apagada).
// O processamento continua durante o encerramento, até ProcessTimeout.
func (c *Consumer) handle(parent context.Context, m types.Message) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), c.cfg.ProcessTimeout)
	defer cancel()
	log := c.log.With(slog.String("sqsMessageId", aws.ToString(m.MessageId)))

	cmd, err := c.command(ctx, m)
	if err == nil {
		var result app.SubmitWagerResult
		result, err = c.svc.SubmitWager(ctx, cmd)
		if err == nil {
			log.Info("mensagem processada",
				slog.String("transactionId", result.Transaction.ID().String()),
				slog.String("status", string(result.Transaction.Status())),
				slog.Bool("replay", result.Replay),
				slog.String("correlationId", cmd.CorrelationID))
			return c.delete(ctx, m, log)
		}
	}

	if code, permanent := classify(err); permanent {
		log.Warn("mensagem com falha permanente enviada à DLQ",
			slog.String("failureCode", code), slog.String("reason", err.Error()))
		if err := c.deadLetter(ctx, m, code, err.Error()); err != nil {
			log.Error("falha ao enviar à DLQ; mensagem será reentregue", slog.String("error", err.Error()))
			return false
		}
		return c.delete(ctx, m, log)
	}
	log.Warn("falha transitória; mensagem será reentregue", slog.String("error", err.Error()))
	return false
}

type inboundEnvelope struct {
	MessageID     string      `json:"messageId"`
	Type          string      `json:"type"`
	OccurredAt    time.Time   `json:"occurredAt"`
	CorrelationID string      `json:"correlationId,omitempty"`
	Data          inboundData `json:"data"`
}

type inboundData struct {
	IdempotencyKey                 string       `json:"idempotencyKey"`
	ProviderID                     string       `json:"providerId"`
	ExternalTransactionID          string       `json:"externalTransactionId"`
	PlayerID                       string       `json:"playerId"`
	WalletID                       string       `json:"walletId"`
	RoundID                        string       `json:"roundId"`
	GameID                         string       `json:"gameId"`
	Kind                           string       `json:"kind"`
	Money                          inboundMoney `json:"money"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
}

// inboundMoney recebe amount como string: um número JSON falha na
// decodificação e nunca passa por float.
type inboundMoney struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// errUnauthenticated: token ausente ou recusado pelo verificador.
var errUnauthenticated = errors.New("unauthenticated")

func (c *Consumer) command(ctx context.Context, m types.Message) (app.SubmitWagerCommand, error) {
	body := aws.ToString(m.Body)
	var env inboundEnvelope
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return app.SubmitWagerCommand{}, fmt.Errorf("%w: envelope: %v", app.ErrInvalidInput, err)
	}
	if dec.More() {
		return app.SubmitWagerCommand{}, fmt.Errorf("%w: envelope com conteúdo extra", app.ErrInvalidInput)
	}
	if env.Type != MessageType {
		return app.SubmitWagerCommand{}, fmt.Errorf("%w: type %q não suportado", app.ErrInvalidInput, env.Type)
	}
	if env.MessageID == "" {
		return app.SubmitWagerCommand{}, fmt.Errorf("%w: messageId obrigatório", app.ErrInvalidInput)
	}

	var token string
	if attr, ok := m.MessageAttributes[AuthorizationAttribute]; ok {
		token, _ = strings.CutPrefix(aws.ToString(attr.StringValue), "Bearer ")
	}
	principal, err := c.auth.Authenticate(ctx, strings.TrimSpace(token))
	if err != nil {
		return app.SubmitWagerCommand{}, fmt.Errorf("%w: %v", errUnauthenticated, err)
	}

	sum := sha256.Sum256([]byte(body))
	correlation := env.CorrelationID
	if correlation == "" {
		correlation = env.MessageID
	}
	d := env.Data
	return app.SubmitWagerCommand{
		Principal:                      principal,
		IdempotencyKey:                 d.IdempotencyKey,
		ProviderID:                     d.ProviderID,
		ExternalTransactionID:          d.ExternalTransactionID,
		PlayerID:                       d.PlayerID,
		WalletID:                       d.WalletID,
		RoundID:                        d.RoundID,
		GameID:                         d.GameID,
		Kind:                           d.Kind,
		Amount:                         d.Money.Amount,
		Currency:                       d.Money.Currency,
		ReferenceExternalTransactionID: d.ReferenceExternalTransactionID,
		CorrelationID:                  correlation,
		CausationID:                    env.MessageID,
		Inbox: &app.InboxMessage{
			ConsumerName: ConsumerName,
			MessageID:    env.MessageID,
			PayloadHash:  hex.EncodeToString(sum[:]),
		},
	}, nil
}

// classify separa falhas permanentes (não adianta repetir) das transitórias.
func classify(err error) (code string, permanent bool) {
	switch {
	case errors.Is(err, errUnauthenticated):
		return "UNAUTHENTICATED", true
	case errors.Is(err, app.ErrInvalidInput):
		return "INVALID_INPUT", true
	case errors.Is(err, app.ErrForbidden):
		return "FORBIDDEN", true
	case errors.Is(err, app.ErrIdempotencyConflict), errors.Is(err, app.ErrInboxConflict):
		return "IDEMPOTENCY_CONFLICT", true
	case errors.Is(err, app.ErrWalletNotFound):
		return "WALLET_NOT_FOUND", true
	}
	return "", false
}

// deadLetter copia a mensagem para a DLQ com o motivo. O token não é copiado.
func (c *Consumer) deadLetter(ctx context.Context, m types.Message, code, reason string) error {
	if len(reason) > 1000 {
		reason = reason[:1000]
	}
	group := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
	if group == "" {
		group = "dead-letter"
	}
	sum := sha256.Sum256(append([]byte(aws.ToString(m.MessageId)+"\x00"), bytes.TrimSpace([]byte(aws.ToString(m.Body)))...))
	_, err := c.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.cfg.Queues.DLQ),
		MessageBody:            m.Body,
		MessageGroupId:         aws.String(group),
		MessageDeduplicationId: aws.String(hex.EncodeToString(sum[:])),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"failureCode":       {DataType: aws.String("String"), StringValue: aws.String(code)},
			"failureReason":     {DataType: aws.String("String"), StringValue: aws.String(reason)},
			"originalMessageId": {DataType: aws.String("String"), StringValue: m.MessageId},
		},
	})
	return err
}

func (c *Consumer) delete(ctx context.Context, m types.Message, log *slog.Logger) bool {
	_, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.cfg.Queues.Inbound),
		ReceiptHandle: m.ReceiptHandle,
	})
	if err != nil {
		// A operação já foi gravada; a reentrega vira replay pela inbox.
		log.Warn("falha ao apagar mensagem processada", slog.String("error", err.Error()))
		return false
	}
	return true
}

// releaseNow devolve à fila mensagens recebidas e não processadas (grupo
// bloqueado ou encerramento), sem esperar o visibility timeout.
func (c *Consumer) releaseNow(msgs []types.Message) {
	if len(msgs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries := make([]types.ChangeMessageVisibilityBatchRequestEntry, len(msgs))
	for i, m := range msgs {
		entries[i] = types.ChangeMessageVisibilityBatchRequestEntry{
			Id:                aws.String(fmt.Sprint(i)),
			ReceiptHandle:     m.ReceiptHandle,
			VisibilityTimeout: 0,
		}
	}
	if _, err := c.client.ChangeMessageVisibilityBatch(ctx, &sqs.ChangeMessageVisibilityBatchInput{
		QueueUrl: aws.String(c.cfg.Queues.Inbound),
		Entries:  entries,
	}); err != nil {
		c.log.Warn("falha ao devolver mensagens à fila", slog.String("error", err.Error()))
	}
}
