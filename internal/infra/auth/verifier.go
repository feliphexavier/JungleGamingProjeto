// Package auth valida access tokens OAuth 2.0 / OIDC emitidos pelo IdP externo
// (Keycloak) e os converte na identidade usada pelos casos de uso.
package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/feliphexavier/jungleGamingProjeto/internal/app"
)

// Claims do token usadas na autorização.
const (
	// ProviderClaim identifica o provedor. É um claim fixo configurado no client
	// do provedor no IdP: o provedor não consegue alterá-lo.
	ProviderClaim = "provider_id"
	// InternalRole é o papel (realm role) do serviço interno.
	InternalRole = "wallet-internal"
)

// ErrUnauthenticated: token ausente, malformado, com assinatura inválida,
// expirado, de outro emissor ou para outra audiência.
var ErrUnauthenticated = errors.New("unauthenticated")

type Config struct {
	// Issuer é o valor exato esperado no claim iss.
	Issuer string
	// JWKSURL é onde as chaves públicas de assinatura são buscadas.
	JWKSURL string
	// Audience é o valor que precisa constar no claim aud.
	Audience string
}

func (c Config) Validate() error {
	if c.Issuer == "" || c.JWKSURL == "" || c.Audience == "" {
		return errors.New("auth: OIDC_ISSUER, OIDC_JWKS_URL e OIDC_AUDIENCE são obrigatórios")
	}
	return nil
}

// Verifier valida tokens localmente: assinatura (chaves do JWKS, com cache e
// renovação automática quando surge um kid novo), iss, aud e exp.
type Verifier struct {
	verifier *oidc.IDTokenVerifier
}

// NewVerifier não faz chamadas de rede; as chaves são buscadas no primeiro uso.
// ctx controla a vida das buscas de chaves em segundo plano.
func NewVerifier(ctx context.Context, cfg Config) (*Verifier, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	keys := oidc.NewRemoteKeySet(ctx, cfg.JWKSURL)
	v := oidc.NewVerifier(cfg.Issuer, keys, &oidc.Config{
		ClientID:             cfg.Audience,
		SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256, oidc.PS256},
	})
	return &Verifier{verifier: v}, nil
}

type tokenClaims struct {
	ProviderID  string `json:"provider_id"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// Authenticate valida o token e devolve a identidade. O providerId autorizado
// vem exclusivamente do token.
func (v *Verifier) Authenticate(ctx context.Context, rawToken string) (app.Principal, error) {
	if rawToken == "" {
		return app.Principal{}, fmt.Errorf("%w: token ausente", ErrUnauthenticated)
	}
	token, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return app.Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	var claims tokenClaims
	if err := token.Claims(&claims); err != nil {
		return app.Principal{}, fmt.Errorf("%w: claims: %v", ErrUnauthenticated, err)
	}
	return app.Principal{
		ProviderID: claims.ProviderID,
		Internal:   slices.Contains(claims.RealmAccess.Roles, InternalRole),
	}, nil
}
