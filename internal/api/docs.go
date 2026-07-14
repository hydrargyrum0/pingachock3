package api

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.yaml
var OpenAPISpec []byte

// swaggerUIHTML loads Swagger UI from a CDN rather than vendoring it -
// /docs is for the operator's own browser (which has normal internet
// access), not for anything running inside Turkmenistan, so there's no
// reason to bloat the binary/image with vendored JS.
const swaggerUIHTML = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>pingachock API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.onload = () => {
      window.ui = SwaggerUIBundle({
        url: '/docs/openapi.yaml',
        dom_id: '#swagger-ui',
        deepLinking: true,
        presets: [SwaggerUIBundle.presets.apis],
      });
    };
  </script>
</body>
</html>`

// ServeDocsUI serves the interactive Swagger UI at /docs - "Authorize" lets
// whoever's looking plug in a real api_key/node secret/admin token and
// exercise the live API ("Try it out") straight from the browser.
func ServeDocsUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(swaggerUIHTML))
}

func ServeOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Write(OpenAPISpec)
}
