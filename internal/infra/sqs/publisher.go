package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
)

// Publisher envia eventos da outbox para uma fila FIFO.
//
//   - MessageGroupId = carteira: o SQS entrega os eventos de cada carteira na
//     ordem de envio.
//   - MessageDeduplicationId = eventId: reenvios do mesmo evento dentro da
//     janela de deduplicação do SQS (5 minutos) são descartados; fora dela, o
//     consumidor deduplica pelo eventId do envelope.
type Publisher struct {
	client *sqs.Client
	queues *Queues
}

func NewPublisher(client *sqs.Client, queues *Queues) *Publisher {
	return &Publisher{client: client, queues: queues}
}

var _ app.EventPublisher = (*Publisher)(nil)

func (p *Publisher) Publish(ctx context.Context, m app.OutboxMessage) error {
	_, err := p.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(p.queues.Events),
		MessageBody:            aws.String(string(m.Payload)),
		MessageGroupId:         aws.String(m.GroupID),
		MessageDeduplicationId: aws.String(m.ID.String()),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"eventType": {DataType: aws.String("String"), StringValue: aws.String(m.EventType)},
			"eventId":   {DataType: aws.String("String"), StringValue: aws.String(m.ID.String())},
		},
	})
	if err != nil {
		return fmt.Errorf("sqs: publicar: %w", err)
	}
	return nil
}
