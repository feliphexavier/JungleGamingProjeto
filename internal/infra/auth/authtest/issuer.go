// Package authtest oferece um emissor de tokens local para testes da
// validação de JWT (expiração, assinatura, emissor, audiência). Os fluxos
// autenticados de ponta a ponta usam o Keycloak real.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth"
)

const Audience = "wallet-api"

// Claims são as claims emitidas pelo IdP de teste, no formato do Keycloak.
type Claims struct {
	jwt.Claims
	ProviderID  string       `json:"provider_id,omitempty"`
	RealmAccess *RealmAccess `json:"realm_access,omitempty"`
}

type RealmAccess struct {
	Roles []string `json:"roles"`
}

// Issuer publica um JWKS em um servidor HTTP local e assina tokens.
type Issuer struct {
	Issuer string
	KeyID  string

	mu     sync.Mutex
	key    *rsa.PrivateKey
	server *httptest.Server
}

func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	iss := &Issuer{}
	iss.RotateKey(t)
	iss.server = httptest.NewServer(http.HandlerFunc(iss.serveJWKS))
	t.Cleanup(iss.server.Close)
	iss.Issuer = iss.server.URL + "/realms/wallet"
	return iss
}

func (i *Issuer) Config() auth.Config {
	return auth.Config{Issuer: i.Issuer, JWKSURL: i.server.URL + "/certs", Audience: Audience}
}

// RotateKey troca a chave de assinatura, como numa rotação do IdP.
func (i *Issuer) RotateKey(t testing.TB) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.key = key
	i.KeyID = hex.EncodeToString(b)
}

func (i *Issuer) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	defer i.mu.Unlock()
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &i.key.PublicKey, KeyID: i.KeyID, Algorithm: string(jose.RS256), Use: "sig",
	}}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(set)
}

// ProviderToken emite um token válido de provedor.
func (i *Issuer) ProviderToken(t testing.TB, providerID string) string {
	t.Helper()
	c := i.base()
	c.ProviderID = providerID
	return i.Sign(t, c)
}

// InternalToken emite um token válido do serviço interno.
func (i *Issuer) InternalToken(t testing.TB) string {
	t.Helper()
	c := i.base()
	c.RealmAccess = &RealmAccess{Roles: []string{auth.InternalRole}}
	return i.Sign(t, c)
}

// UnprivilegedToken emite um token válido sem provider_id nem papel interno.
func (i *Issuer) UnprivilegedToken(t testing.TB) string {
	t.Helper()
	return i.Sign(t, i.base())
}

// ExpiredToken emite um token de provedor já expirado.
func (i *Issuer) ExpiredToken(t testing.TB, providerID string) string {
	t.Helper()
	c := i.base()
	c.ProviderID = providerID
	c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-10 * time.Minute))
	c.Expiry = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	return i.Sign(t, c)
}

func (i *Issuer) base() Claims {
	now := time.Now()
	return Claims{Claims: jwt.Claims{
		Issuer:   i.Issuer,
		Audience: jwt.Audience{Audience},
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(5 * time.Minute)),
	}}
}

// Sign assina claims arbitrárias com a chave atual.
func (i *Issuer) Sign(t testing.TB, c Claims) string {
	t.Helper()
	i.mu.Lock()
	key, kid := i.key, i.KeyID
	i.mu.Unlock()
	return SignWith(t, key, kid, c)
}

// SignWith assina com uma chave qualquer (para simular assinatura inválida).
func SignWith(t testing.TB, key *rsa.PrivateKey, kid string, c Claims) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(signer).Claims(c).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
