package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OutboxMessage é um evento pendente reservado para publicação.
type OutboxMessage struct {
	ID        uuid.UUID
	GroupID   string // carteira: chave de ordenação na fila
	EventType string
	Payload   []byte // envelope imutável gravado na outbox
	Attempts  int
	Seq       int64
}

// EventPublisher envia um evento ao broker. Deve ser idempotente pelo ID do
// evento: a mesma mensagem pode ser enviada mais de uma vez.
type EventPublisher interface {
	Publish(ctx context.Context, m OutboxMessage) error
}

// OutboxRelayConfig controla a publicação.
type OutboxRelayConfig struct {
	// Owner identifica a instância nos leases (ex.: hostname + pid).
	Owner string
	// BatchSize é o máximo de eventos reservados por ciclo.
	BatchSize int
	// Lease é por quanto tempo a reserva vale; expirada, outra instância assume.
	Lease time.Duration
	// MaxBackoff limita o intervalo entre tentativas de um evento que falhou.
	MaxBackoff time.Duration
}

// OutboxRelay publica os eventos da outbox depois do commit que os gravou.
//
// Garantias:
//   - Nenhum evento é publicado antes do commit: só linhas commitadas são lidas.
//   - Entrega pelo menos uma vez: o evento só é marcado publicado depois de o
//     broker confirmar; uma queda entre as duas etapas causa reenvio com o
//     mesmo eventId.
//   - Ordem por carteira: só é reservado um prefixo contínuo dos pendentes de
//     cada carteira, e nenhuma carteira fica com eventos reservados por duas
//     instâncias ao mesmo tempo. Se um evento falha, os seguintes da mesma
//     carteira esperam por ele.
type OutboxRelay struct {
	store Store
	pub   EventPublisher
	clock Clock
	cfg   OutboxRelayConfig
}

func NewOutboxRelay(store Store, pub EventPublisher, cfg OutboxRelayConfig, opts ...RelayOption) *OutboxRelay {
	r := &OutboxRelay{store: store, pub: pub, clock: SystemClock{}, cfg: cfg}
	for _, o := range opts {
		o(r)
	}
	return r
}

type RelayOption func(*OutboxRelay)

func WithRelayClock(c Clock) RelayOption { return func(r *OutboxRelay) { r.clock = c } }

// finishTimeout é o prazo para concluir a publicação em andamento depois que o
// contexto foi cancelado (encerramento).
const finishTimeout = 5 * time.Second

// PublishBatch reserva e publica um lote. Devolve quantos eventos foram
// publicados.
func (r *OutboxRelay) PublishBatch(ctx context.Context) (int, error) {
	var batch []OutboxMessage
	err := r.store.InTx(ctx, func(ctx context.Context, repos Repos) error {
		var err error
		batch, err = repos.Outbox().Claim(ctx, r.cfg.Owner, r.clock.Now(), r.cfg.Lease, r.cfg.BatchSize)
		return err
	})
	if err != nil || len(batch) == 0 {
		return 0, err
	}

	// A publicação e a marcação de cada evento terminam mesmo durante o
	// encerramento; o que ainda não começou é devolvido.
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.Lease)
	defer cancel()
	outbox := r.store.Reads().Outbox()

	var (
		published int
		blocked   = map[string]bool{}
		release   []uuid.UUID
		errs      []error
	)
	for _, m := range batch {
		if blocked[m.GroupID] || ctx.Err() != nil {
			release = append(release, m.ID)
			continue
		}
		if err := r.pub.Publish(work, m); err != nil {
			blocked[m.GroupID] = true
			next := r.clock.Now().Add(r.backoff(m.Attempts + 1))
			if mErr := outbox.MarkFailed(work, m.ID, r.cfg.Owner, next, err.Error()); mErr != nil {
				errs = append(errs, mErr)
			}
			errs = append(errs, fmt.Errorf("publicar evento %s: %w", m.ID, err))
			continue
		}
		if err := outbox.MarkPublished(work, m.ID, r.cfg.Owner, r.clock.Now()); err != nil {
			// Publicado mas não marcado: será reenviado com o mesmo eventId.
			blocked[m.GroupID] = true
			errs = append(errs, err)
			continue
		}
		published++
	}
	if len(release) > 0 {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		defer rcancel()
		if err := outbox.Release(rctx, release, r.cfg.Owner); err != nil {
			errs = append(errs, err)
		}
	}
	return published, errors.Join(errs...)
}

// backoff exponencial a partir de 1s, limitado a MaxBackoff.
func (r *OutboxRelay) backoff(attempts int) time.Duration {
	d := time.Second
	for i := 1; i < attempts && d < r.cfg.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, r.cfg.MaxBackoff)
}
