package app

import (
	"context"
	"errors"

	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
)

// ResumePendingReferences retoma até limit operações em PENDING_REFERENCE com
// próxima tentativa vencida. Cada uma é processada em sua própria transação
// SQL; a seleção usa SKIP LOCKED, então várias instâncias podem rodar ao mesmo
// tempo sem pegar a mesma operação. Devolve quantas foram retomadas.
func (s *Service) ResumePendingReferences(ctx context.Context, limit int) (int, error) {
	resumed, conflicts := 0, 0
	for resumed < limit {
		did, err := s.resumeOne(ctx)
		if errors.Is(err, ErrConcurrentUpdate) && conflicts < maxRaceRetries {
			conflicts++ // deadlock ou disputa detectada pelo banco: tenta de novo
			continue
		}
		if err != nil {
			return resumed, err
		}
		if !did {
			break
		}
		resumed++
	}
	return resumed, nil
}

func (s *Service) resumeOne(ctx context.Context) (bool, error) {
	did := false
	err := s.store.InTx(ctx, func(ctx context.Context, r Repos) error {
		now := s.clock.Now()
		claimed, err := r.Transactions().ClaimDuePendingReferences(ctx, now, 1)
		if err != nil || len(claimed) == 0 {
			return err
		}
		tx := claimed[0]

		wallet, err := r.Wallets().GetForUpdate(ctx, tx.WalletID())
		if err != nil {
			return err
		}
		did = true
		meta := domain.EventMeta{CorrelationID: tx.IdempotencyKey(), CausationID: tx.ID().String()}
		return s.process(ctx, r, tx, wallet, false, meta, now)
	})
	return did, err
}
