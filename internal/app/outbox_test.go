package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/postgres"
)

// recordingPublisher registra os envios e falha para os grupos em failGroups.
// O broker real é coberto em internal/infra/sqs; aqui o foco é a regra de
// reserva, falha e ordem do relay sobre o PostgreSQL real.
type recordingPublisher struct {
	mu         sync.Mutex
	failGroups map[string]bool
	sent       []app.OutboxMessage
}

func (p *recordingPublisher) Publish(_ context.Context, m app.OutboxMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failGroups[m.GroupID] {
		return errors.New("broker indisponível")
	}
	p.sent = append(p.sent, m)
	return nil
}

func (p *recordingPublisher) groups() map[string][]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string][]int64{}
	for _, m := range p.sent {
		out[m.GroupID] = append(out[m.GroupID], m.Seq)
	}
	return out
}

func TestOutboxRelayFailureKeepsWalletOrder(t *testing.T) {
	e := setup(t)
	w1, w2 := e.openWallet("100.00"), e.openWallet("100.00")
	for _, ext := range []string{"a", "b"} {
		e.submit(wager(w1, "BET", "1.00", "w1-"+ext, ""))
		e.submit(wager(w2, "BET", "1.00", "w2-"+ext, ""))
	}

	pub := &recordingPublisher{failGroups: map[string]bool{w1.ID().String(): true}}
	relay := app.NewOutboxRelay(postgres.NewStore(e.db.Pool), pub, app.OutboxRelayConfig{
		Owner: "relay-1", BatchSize: 100, Lease: 30 * time.Second, MaxBackoff: time.Minute,
	}, app.WithRelayClock(e.clock))

	n, err := relay.PublishBatch(e.ctx)
	if err == nil {
		t.Error("falha de publicação deveria ser reportada")
	}
	w1Events := e.count(`SELECT count(*) FROM outbox_events WHERE message_group_id = $1`, w1.ID().String())
	w2Events := e.count(`SELECT count(*) FROM outbox_events WHERE message_group_id = $1`, w2.ID().String())
	if n != w2Events {
		t.Errorf("publicados = %d, want %d (todos os da carteira 2)", n, w2Events)
	}
	// Carteira 1: só o primeiro evento foi tentado; os seguintes esperam.
	if got := e.count(`SELECT count(*) FROM outbox_events WHERE message_group_id = $1 AND attempts = 1`, w1.ID().String()); got != 1 {
		t.Errorf("eventos da carteira 1 com tentativa = %d, want 1", got)
	}
	if got := e.count(`SELECT count(*) FROM outbox_events WHERE locked_by IS NOT NULL`); got != 0 {
		t.Errorf("%d reservas não liberadas", got)
	}

	// Antes do backoff vencer, nada da carteira 1 é reservado.
	pub.failGroups = nil
	if n, err := relay.PublishBatch(e.ctx); err != nil || n != 0 {
		t.Errorf("antes do backoff: publicados = %d, err = %v", n, err)
	}

	e.clock.Advance(2 * time.Second)
	if n, err := relay.PublishBatch(e.ctx); err != nil || n != w1Events {
		t.Errorf("após o backoff: publicados = %d, err = %v; want %d", n, err, w1Events)
	}
	if got := e.count(`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`); got != 0 {
		t.Errorf("%d eventos pendentes", got)
	}
	for group, seqs := range pub.groups() {
		for i := 1; i < len(seqs); i++ {
			if seqs[i] <= seqs[i-1] {
				t.Errorf("carteira %s publicada fora de ordem: %v", group, seqs)
			}
		}
	}
}

// Um evento publicado não pode ser republicado nem ter o conteúdo alterado.
func TestOutboxEventImmutableAfterPublish(t *testing.T) {
	e := setup(t)
	w := e.openWallet("100.00")
	e.submit(wager(w, "BET", "1.00", "x", ""))
	relay := app.NewOutboxRelay(postgres.NewStore(e.db.Pool), &recordingPublisher{}, app.OutboxRelayConfig{
		Owner: "r", BatchSize: 10, Lease: time.Minute, MaxBackoff: time.Minute,
	}, app.WithRelayClock(e.clock))
	if _, err := relay.PublishBatch(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(e.ctx, `UPDATE outbox_events SET published_at = NULL`); err == nil {
		t.Error("evento publicado voltou a pendente")
	}
	if _, err := e.db.Pool.Exec(e.ctx, `UPDATE outbox_events SET payload = '{}'`); err == nil {
		t.Error("payload do evento foi alterado")
	}
}
