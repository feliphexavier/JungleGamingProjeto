package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"

	"github.com/feliphexavier/jungleGamingProjeto/api"
	"github.com/feliphexavier/jungleGamingProjeto/internal/infra/httpapi"
)

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	spec, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	if err != nil {
		t.Fatalf("openapi.yaml não carrega: %v", err)
	}
	if err := spec.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml inválido: %v", err)
	}
	return spec
}

// A especificação documenta exatamente as rotas registradas no roteador.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	spec := loadSpec(t)
	server := httpapi.NewServer(nil, nil, nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	undocumented := map[string]bool{"GET /openapi.yaml": true, "GET /docs": true}
	var routes []string
	err := chi.Walk(server.Handler().(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + strings.TrimSuffix(route, "/")
		if !undocumented[key] {
			routes = append(routes, key)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var documented []string
	for path, item := range spec.Paths.Map() {
		for method := range item.Operations() {
			documented = append(documented, method+" "+path)
		}
	}
	sort.Strings(routes)
	sort.Strings(documented)
	if strings.Join(routes, "\n") != strings.Join(documented, "\n") {
		t.Errorf("rotas e documentação divergem\nrotas:\n  %s\ndocumentadas:\n  %s",
			strings.Join(routes, "\n  "), strings.Join(documented, "\n  "))
	}
}

// Toda operação autenticada documenta sucesso e os erros comuns.
func TestOpenAPIDocumentsErrors(t *testing.T) {
	spec := loadSpec(t)
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			public := op.Security != nil && len(*op.Security) == 0
			hasSuccess := false
			for code := range op.Responses.Map() {
				if strings.HasPrefix(code, "2") {
					hasSuccess = true
				}
			}
			if !hasSuccess {
				t.Errorf("%s %s sem resposta de sucesso", method, path)
			}
			if public {
				continue
			}
			for _, code := range []string{"401", "403", "500", "503"} {
				if op.Responses.Value(code) == nil {
					t.Errorf("%s %s não documenta %s", method, path, code)
				}
			}
		}
	}
}

func TestDocsEndpoints(t *testing.T) {
	server := httpapi.NewServer(nil, nil, nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()

	for path, contentType := range map[string]string{"/openapi.yaml": "application/yaml", "/docs": "text/html"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), contentType) {
			t.Errorf("GET %s = %d %s", path, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
}
