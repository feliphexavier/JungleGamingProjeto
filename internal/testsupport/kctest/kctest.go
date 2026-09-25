// Package kctest obtém tokens reais do Keycloak do Docker Compose para testes
// de integração com o IdP.
//
// Variáveis:
//   - TEST_KEYCLOAK_URL: URL base do Keycloak alcançável pelo teste
//     (ex.: http://localhost:8081). Sem ela, o teste é pulado.
//   - TEST_OIDC_ISSUER: emissor esperado nos tokens (padrão
//     http://localhost:8081/realms/wallet, fixado por KC_HOSTNAME no Compose).
package kctest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth"
)

const (
	EnvURL    = "TEST_KEYCLOAK_URL"
	EnvIssuer = "TEST_OIDC_ISSUER"
	Realm     = "wallet"
	Audience  = "wallet-api"
)

// Keycloak aponta para uma instância real.
type Keycloak struct {
	BaseURL string
	Issuer  string
}

// New pula o teste se TEST_KEYCLOAK_URL não estiver definida.
func New(t testing.TB) *Keycloak {
	t.Helper()
	base := strings.TrimRight(os.Getenv(EnvURL), "/")
	if base == "" {
		t.Skipf("%s não definida: teste com Keycloak real pulado", EnvURL)
	}
	issuer := os.Getenv(EnvIssuer)
	if issuer == "" {
		issuer = "http://localhost:8081/realms/" + Realm
	}
	return &Keycloak{BaseURL: base, Issuer: issuer}
}

// Config devolve a configuração de validação apontando para o JWKS real.
func (k *Keycloak) Config() auth.Config {
	return auth.Config{
		Issuer:   k.Issuer,
		JWKSURL:  k.BaseURL + "/realms/" + Realm + "/protocol/openid-connect/certs",
		Audience: Audience,
	}
}

// Token obtém um access token por client_credentials. Falha o teste se o
// Keycloak recusar.
func (k *Keycloak) Token(t testing.TB, clientID, secret string) string {
	t.Helper()
	tok, status, err := k.RequestToken(clientID, secret)
	if err != nil || status != http.StatusOK {
		t.Fatalf("token para %s: status %d, %v", clientID, status, err)
	}
	return tok
}

// RequestToken pede um token e devolve também o status HTTP do Keycloak.
func (k *Keycloak) RequestToken(clientID, secret string) (string, int, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	}
	res, err := http.PostForm(k.BaseURL+"/realms/"+Realm+"/protocol/openid-connect/token", form)
	if err != nil {
		return "", 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", res.StatusCode, nil
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", res.StatusCode, fmt.Errorf("resposta do Keycloak: %w", err)
	}
	return body.AccessToken, res.StatusCode, nil
}
