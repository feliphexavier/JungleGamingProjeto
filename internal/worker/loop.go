// Package worker executa tarefas periódicas com início, parada e término
// observáveis.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Loop repete uma tarefa em intervalo fixo até ser parado.
//
// Stop cancela o contexto (a tarefa em andamento recebe o cancelamento e não
// busca trabalho novo) e espera a goroutine terminar, respeitando o prazo do
// contexto de parada. Stopped fica fechado quando a goroutine terminou.
type Loop struct {
	name     string
	interval time.Duration
	task     func(ctx context.Context) error
	log      *slog.Logger

	mu      sync.Mutex
	cancel  context.CancelFunc
	stopped chan struct{}
}

func NewLoop(name string, interval time.Duration, task func(ctx context.Context) error, log *slog.Logger) *Loop {
	return &Loop{name: name, interval: interval, task: task, log: log.With(slog.String("worker", name))}
}

// Start inicia a goroutine. Chamado pelo ciclo de vida da aplicação.
func (l *Loop) Start(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancel != nil {
		return errors.New("worker " + l.name + " já iniciado")
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.stopped = make(chan struct{})
	go l.run(ctx)
	l.log.Info("worker iniciado")
	return nil
}

func (l *Loop) run(ctx context.Context) {
	defer close(l.stopped)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		if err := l.task(ctx); err != nil && !errors.Is(err, context.Canceled) {
			l.log.Error("falha na execução do worker", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Stop sinaliza a parada e espera o término até o prazo de ctx.
func (l *Loop) Stop(ctx context.Context) error {
	l.mu.Lock()
	cancel, stopped := l.cancel, l.stopped
	l.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-stopped:
		l.log.Info("worker encerrado")
		return nil
	case <-ctx.Done():
		l.log.Error("worker não terminou dentro do prazo de encerramento")
		return ctx.Err()
	}
}

// Stopped fecha quando a goroutine do worker terminou.
func (l *Loop) Stopped() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopped
}
