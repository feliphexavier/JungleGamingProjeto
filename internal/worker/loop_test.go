package worker_test

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feliphexavier/jungleGamingProjeto/internal/worker"
)

var discard = slog.New(slog.NewJSONHandler(io.Discard, nil))

func TestLoopRunsAndStops(t *testing.T) {
	var runs atomic.Int32
	loop := worker.NewLoop("test", 10*time.Millisecond, func(context.Context) error {
		runs.Add(1)
		return nil
	}, discard)
	if err := loop.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if err := loop.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runs.Load() < 2 {
		t.Errorf("execuções = %d, esperava várias", runs.Load())
	}
	after := runs.Load()
	time.Sleep(30 * time.Millisecond)
	if runs.Load() != after {
		t.Error("worker continuou executando após Stop")
	}
}

// Stop cancela o contexto da tarefa em andamento e espera ela terminar.
func TestStopWaitsForInFlightTask(t *testing.T) {
	started := make(chan struct{})
	var finished atomic.Bool
	loop := worker.NewLoop("test", time.Hour, func(ctx context.Context) error {
		close(started)
		<-ctx.Done() // tarefa longa que respeita o cancelamento
		time.Sleep(20 * time.Millisecond)
		finished.Store(true)
		return ctx.Err()
	}, discard)
	if err := loop.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := loop.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !finished.Load() {
		t.Error("Stop retornou antes de a tarefa terminar")
	}
}

// Se a tarefa não termina no prazo, Stop devolve erro em vez de travar.
func TestStopRespectsDeadline(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	loop := worker.NewLoop("test", time.Hour, func(context.Context) error {
		close(started)
		<-release // ignora o cancelamento
		return nil
	}, discard)
	if err := loop.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := loop.Stop(ctx); err == nil {
		t.Error("Stop deveria falhar ao estourar o prazo")
	}
	close(release)
	<-loop.Stopped()
}
