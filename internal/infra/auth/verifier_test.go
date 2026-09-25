package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/auth/authtest"
)

func TestAuthenticate(t *testing.T) {
	idp := authtest.NewIssuer(t)
	v, err := auth.NewVerifier(context.Background(), idp.Config())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	p, err := v.Authenticate(ctx, idp.ProviderToken(t, "provider-a"))
	if err != nil || p.ProviderID != "provider-a" || p.Internal {
		t.Errorf("token de provedor = %+v, %v", p, err)
	}

	p, err = v.Authenticate(ctx, idp.InternalToken(t))
	if err != nil || !p.Internal || p.ProviderID != "" {
		t.Errorf("token interno = %+v, %v", p, err)
	}
}

func TestAuthenticateRejects(t *testing.T) {
	idp := authtest.NewIssuer(t)
	v, err := auth.NewVerifier(context.Background(), idp.Config())
	if err != nil {
		t.Fatal(err)
	}

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	valid := func() authtest.Claims {
		return authtest.Claims{
			Claims: jwt.Claims{
				Issuer:   idp.Issuer,
				Audience: jwt.Audience{authtest.Audience},
				Expiry:   jwt.NewNumericDate(now.Add(5 * time.Minute)),
				IssuedAt: jwt.NewNumericDate(now),
			},
			ProviderID: "provider-a",
		}
	}

	tests := []struct {
		name  string
		token func() string
	}{
		{"ausente", func() string { return "" }},
		{"lixo", func() string { return "nao-e-um-jwt" }},
		{"expirado", func() string {
			c := valid()
			c.Expiry = jwt.NewNumericDate(now.Add(-time.Minute))
			c.IssuedAt = jwt.NewNumericDate(now.Add(-10 * time.Minute))
			return idp.Sign(t, c)
		}},
		{"outro emissor", func() string {
			c := valid()
			c.Issuer = "https://evil.example/realms/wallet"
			return idp.Sign(t, c)
		}},
		{"outra audiência", func() string {
			c := valid()
			c.Audience = jwt.Audience{"account"}
			return idp.Sign(t, c)
		}},
		{"assinado por outra chave", func() string {
			return authtest.SignWith(t, otherKey, idp.KeyID, valid())
		}},
		{"alg none", func() string {
			header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
			payload, _ := json.Marshal(valid())
			return b64(header) + "." + b64(payload) + "."
		}},
		{"payload adulterado", func() string {
			tok := idp.Sign(t, valid())
			c := valid()
			c.ProviderID = "provider-b"
			forged := idp.Sign(t, c)
			// cabeçalho e assinatura do original com o payload de outro token
			return splitJWT(tok)[0] + "." + splitJWT(forged)[1] + "." + splitJWT(tok)[2]
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := v.Authenticate(context.Background(), tt.token())
			if !errors.Is(err, auth.ErrUnauthenticated) {
				t.Errorf("err = %v, want ErrUnauthenticated", err)
			}
			if p.ProviderID != "" || p.Internal {
				t.Errorf("identidade concedida a token inválido: %+v", p)
			}
		})
	}
}

func TestKeyRotation(t *testing.T) {
	idp := authtest.NewIssuer(t)
	v, err := auth.NewVerifier(context.Background(), idp.Config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Authenticate(context.Background(), idp.ProviderToken(t, "provider-a")); err != nil {
		t.Fatal(err)
	}
	idp.RotateKey(t)
	if _, err := v.Authenticate(context.Background(), idp.ProviderToken(t, "provider-a")); err != nil {
		t.Errorf("token com chave nova após rotação: %v", err)
	}
}

func TestConfigValidate(t *testing.T) {
	if _, err := auth.NewVerifier(context.Background(), auth.Config{}); err == nil {
		t.Error("configuração vazia deveria falhar")
	}
}

func TestJWKSUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	idp := authtest.NewIssuer(t)
	cfg := idp.Config()
	cfg.JWKSURL = srv.URL
	v, err := auth.NewVerifier(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Authenticate(context.Background(), idp.ProviderToken(t, "provider-a")); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("JWKS indisponível = %v, want ErrUnauthenticated", err)
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func splitJWT(tok string) []string { return strings.Split(tok, ".") }
