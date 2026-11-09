package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestOpenAPIRoutes holds openapi.yaml and the routes of New to each other: every method and
// path the server serves is in the document, and the document has no other.
func TestOpenAPIRoutes(t *testing.T) {
	var doc struct {
		OpenAPI string                    `yaml:"openapi"`
		Paths   map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openAPI, &doc); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Errorf("openapi: %q, want 3.1.0", doc.OpenAPI)
	}
	var documented []string
	for path, item := range doc.Paths {
		for method := range item {
			if method != "parameters" {
				documented = append(documented, strings.ToUpper(method)+" "+path)
			}
		}
	}
	served := []string{"GET /api/v1/openapi.yaml"}
	for _, rt := range (&handler{}).routes() {
		served = append(served, rt.pattern)
	}
	slices.Sort(documented)
	slices.Sort(served)
	if !slices.Equal(documented, served) {
		t.Errorf("openapi.yaml describes\n\t%s\nthe server serves\n\t%s", strings.Join(documented, "\n\t"), strings.Join(served, "\n\t"))
	}
}

// TestOpenAPIServed checks that anyone gets the document at /api/v1/openapi.yaml.
func TestOpenAPIServed(t *testing.T) {
	f := newFixture(t, Options{Auth: fakeAuth{}})
	rr := f.do(http.MethodGet, "/api/v1/openapi.yaml", "", auth.Anonymous)
	wantStatus(t, rr, http.StatusOK)
	if ct := rr.Header().Get("Content-Type"); ct != "application/yaml" || !strings.HasPrefix(rr.Body.String(), "openapi: 3.1.0") {
		t.Errorf("Content-Type %q, body %.20q…; want the document as application/yaml", ct, rr.Body.String())
	}
}
