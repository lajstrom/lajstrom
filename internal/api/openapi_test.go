package api

import (
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
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

// TestErrorCodes holds the codes of the envelope to each other (#579): the enum of the Error
// schema of openapi.yaml is the set of codes codeOf answers, and every status with a code in
// docs/api.md, "| 401 | `UNAUTHENTICATED` |" in the table of Errors or "`401 UNAUTHENTICATED`" in
// the text with a code of the envelope (not one of /v2, SIZE_INVALID), has the code of codeOf.
func TestErrorCodes(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas struct {
				Error struct {
					Properties struct {
						Error struct {
							Properties struct {
								Code struct {
									Enum []string `yaml:"enum"`
								} `yaml:"code"`
							} `yaml:"properties"`
						} `yaml:"error"`
					} `yaml:"properties"`
				} `yaml:"Error"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(openAPI, &doc); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	var answered []string
	for status := 400; status < 600; status++ {
		if c := codeOf(status); !slices.Contains(answered, c) {
			answered = append(answered, c)
		}
	}
	enum := doc.Components.Schemas.Error.Properties.Error.Properties.Code.Enum
	slices.Sort(enum)
	slices.Sort(answered)
	if !slices.Equal(enum, answered) {
		t.Errorf("the codes of openapi.yaml are %v, codeOf answers %v", enum, answered)
	}

	md, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := regexp.MustCompile("(?m)^\\| ([45][0-9][0-9]) \\| `([A-Z_]+)` \\||`([45][0-9][0-9]) ([A-Z_]+)`")
	matches := documented.FindAllStringSubmatch(string(md), -1)
	if len(matches) < 10 {
		t.Fatalf("docs/api.md: %d statuses with a code, want the table of Errors at least", len(matches))
	}
	for _, m := range matches {
		status, code := m[1]+m[3], m[2]+m[4]
		n, _ := strconv.Atoi(status)
		if want := codeOf(n); code != want && (m[1] != "" || slices.Contains(answered, code)) {
			t.Errorf("docs/api.md: %s %s, codeOf(%s) = %s", status, code, status, want)
		}
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
