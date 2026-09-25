package app

import (
	"github.com/google/uuid"

	"github.com/feliphexavier/jungleGamingProjeto/internal/domain"
)

// Principal é a identidade autenticada que executa um caso de uso.
type Principal struct {
	// ProviderID é o provedor autorizado pelo token (vazio para o serviço interno).
	ProviderID string
	// Internal indica o serviço interno, único autorizado a operar carteiras.
	Internal bool
}

// Service reúne os casos de uso.
type Service struct {
	store       Store
	clock       Clock
	newID       func() uuid.UUID
	retryPolicy domain.ReferenceRetryPolicy
}

// Option ajusta o Service (usado principalmente em testes).
type Option func(*Service)

func WithClock(c Clock) Option { return func(s *Service) { s.clock = c } }

func WithRetryPolicy(p domain.ReferenceRetryPolicy) Option {
	return func(s *Service) { s.retryPolicy = p }
}

func NewService(store Store, opts ...Option) *Service {
	s := &Service{
		store:       store,
		clock:       SystemClock{},
		newID:       NewID,
		retryPolicy: domain.DefaultReferenceRetryPolicy,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// maxRaceRetries limita quantas vezes uma operação é refeita após perder uma
// corrida de gravação para outra instância.
const maxRaceRetries = 3
