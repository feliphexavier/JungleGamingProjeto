// Package sqs integra o serviço ao Amazon SQS (LocalStack no ambiente local):
// consumo das operações de provedores e publicação dos eventos da outbox.
package sqs

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// Config identifica o endpoint e as filas. As credenciais vêm da cadeia padrão
// da AWS (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY no ambiente local).
type Config struct {
	// Endpoint sobrescreve o endpoint da AWS (ex.: http://localstack:4566).
	// Vazio usa o endpoint real da região.
	Endpoint string
	Region   string

	InboundQueue string // operações de provedores (FIFO)
	DLQ          string // mensagens com falha permanente (FIFO)
	EventsQueue  string // eventos de integração (FIFO)
}

func (c Config) Validate() error {
	var errs []error
	if c.Region == "" {
		errs = append(errs, errors.New("AWS_REGION é obrigatória"))
	}
	if c.InboundQueue == "" || c.DLQ == "" || c.EventsQueue == "" {
		errs = append(errs, errors.New("SQS_INBOUND_QUEUE, SQS_DLQ e SQS_EVENTS_QUEUE são obrigatórias"))
	}
	return errors.Join(errs...)
}

// NewClient cria o cliente SQS. Não faz chamadas de rede.
func NewClient(ctx context.Context, cfg Config) (*sqs.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("sqs: configuração da AWS: %w", err)
	}
	return sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	}), nil
}

// QueueURL resolve o nome da fila. Falha se a fila não existir.
func QueueURL(ctx context.Context, c *sqs.Client, name string) (string, error) {
	out, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		return "", fmt.Errorf("sqs: fila %q: %w", name, err)
	}
	return aws.ToString(out.QueueUrl), nil
}

// Queues guarda as URLs das filas, resolvidas na subida (Resolve). Publisher e
// Consumer leem as URLs no momento do uso.
type Queues struct {
	Inbound string
	DLQ     string
	Events  string
}

// Resolve obtém as URLs; falha se alguma fila não existir.
func (q *Queues) Resolve(ctx context.Context, c *sqs.Client, cfg Config) error {
	var err error
	if q.Inbound, err = QueueURL(ctx, c, cfg.InboundQueue); err != nil {
		return err
	}
	if q.DLQ, err = QueueURL(ctx, c, cfg.DLQ); err != nil {
		return err
	}
	q.Events, err = QueueURL(ctx, c, cfg.EventsQueue)
	return err
}
