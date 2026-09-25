package httpapi

import (
	"net/http"

	"github.com/feliphexavier/jungleGamingProjeto/api"
)

// swaggerUIVersion fixa a versão do Swagger UI carregado pela página /docs.
const swaggerUIVersion = "5.17.14"

// docsPage carrega o Swagger UI apontando para /openapi.yaml. A página é
// servida pela própria API, então as chamadas do "Try it out" são da mesma
// origem e não dependem de CORS.
const docsPage = `<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Jungle Wallet API</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui-bundle.js"></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: "/openapi.yaml",
      dom_id: "#swagger-ui",
      persistAuthorization: true,
      displayRequestDuration: true,
      tryItOutEnabled: true
    });
  </script>
</body>
</html>`

func (s *Server) openAPISpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(api.OpenAPI)
}

func (s *Server) docs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(docsPage))
}
