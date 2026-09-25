// Package sqstest cria filas SQS descartáveis no LocalStack para testes de
// integração.
//
// O endpoint vem de TEST_SQS_ENDPOINT (ex.: http://localhost:4566); sem ela, o
// teste é pulado. Cada chamada a New cria filas FIFO com nomes únicos e as
// remove ao final do teste.
package sqstest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	sqsinfra "github.com/feliphexavier/jungleGamingProjeto/internal/infra/sqs"
)

const EnvEndpoint = "TEST_SQS_ENDPOINT"

type SQS struct {
	Client *sqs.Client
	Config sqsinfra.Config
	Queues *sqsinfra.Queues
}

func New(t testing.TB) *SQS {
	t.Helper()
	endpoint := os.Getenv(EnvEndpoint)
	if endpoint == "" {
		t.Skipf("%s não definida: teste com SQS real pulado", EnvEndpoint)
	}
	// Credenciais fictícias aceitas pelo LocalStack.
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Setenv("AWS_ACCESS_KEY_ID", "test")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	}

	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	id := hex.EncodeToString(suffix)
	cfg := sqsinfra.Config{
		Endpoint:     endpoint,
		Region:       "us-east-1",
		InboundQueue: "test-" + id + "-in.fifo",
		DLQ:          "test-" + id + "-dlq.fifo",
		EventsQueue:  "test-" + id + "-events.fifo",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := sqsinfra.NewClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{cfg.InboundQueue, cfg.DLQ, cfg.EventsQueue} {
		out, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
			QueueName: aws.String(name),
			Attributes: map[string]string{
				string(types.QueueAttributeNameFifoQueue):         "true",
				string(types.QueueAttributeNameVisibilityTimeout): "5",
			},
		})
		if err != nil {
			t.Fatalf("sqstest: criar fila %s: %v", name, err)
		}
		url := aws.ToString(out.QueueUrl)
		t.Cleanup(func() {
			_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(url)})
		})
	}
	queues := &sqsinfra.Queues{}
	if err := queues.Resolve(ctx, client, cfg); err != nil {
		t.Fatal(err)
	}
	return &SQS{Client: client, Config: cfg, Queues: queues}
}

// Send envia uma mensagem FIFO com o token no atributo Authorization (vazio =
// sem atributo).
func (s *SQS) Send(t testing.TB, queueURL, body, group, dedupID, token string) {
	t.Helper()
	in := &sqs.SendMessageInput{
		QueueUrl:               aws.String(queueURL),
		MessageBody:            aws.String(body),
		MessageGroupId:         aws.String(group),
		MessageDeduplicationId: aws.String(dedupID),
	}
	if token != "" {
		in.MessageAttributes = map[string]types.MessageAttributeValue{
			sqsinfra.AuthorizationAttribute: {DataType: aws.String("String"), StringValue: aws.String("Bearer " + token)},
		}
	}
	if _, err := s.Client.SendMessage(context.Background(), in); err != nil {
		t.Fatalf("sqstest: enviar: %v", err)
	}
}

// Drain recebe e apaga todas as mensagens disponíveis até a fila ficar vazia
// por um ciclo de long polling curto. Devolve na ordem recebida.
func (s *SQS) Drain(t testing.TB, queueURL string) []types.Message {
	t.Helper()
	var all []types.Message
	for {
		out, err := s.Client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl:                    aws.String(queueURL),
			MaxNumberOfMessages:         10,
			WaitTimeSeconds:             1,
			MessageAttributeNames:       []string{"All"},
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
		})
		if err != nil {
			t.Fatalf("sqstest: receber: %v", err)
		}
		if len(out.Messages) == 0 {
			return all
		}
		for _, m := range out.Messages {
			all = append(all, m)
			_, _ = s.Client.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
				QueueUrl: aws.String(queueURL), ReceiptHandle: m.ReceiptHandle,
			})
		}
	}
}
