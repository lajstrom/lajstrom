package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// answered holds what the handler answered in the tests of the package, as "<pattern> <status>":
// the pattern of the route in New ("GET /api/v1/repos 400"). Every handler that a test builds
// with New records into it (recordStatuses).
var answered sync.Map

// anyOperation are the statuses that any operation may answer and that openapi.yaml does not
// list on each: a body that stalls (408) or is too large (413), a server error (500) and a
// database that does not answer (503), as the table of Errors of docs/api.md says.
var anyOperation = []string{"408", "413", "500", "503"}

// TestMain runs the tests, then holds openapi.yaml to the statuses that the handler answered in
// them (undocumented): each route's operation lists every status it was seen to answer, so that
// the document says what the handlers do and not only what its author thought of (#714).
func TestMain(m *testing.M) {
	code := m.Run()
	missing, err := undocumented()
	if err == nil && len(missing) > 0 {
		err = fmt.Errorf("the operations do not list statuses that the tests saw them answer (internal/api/statuses_test.go):\n\t%s",
			strings.Join(missing, "\n\t"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi.yaml:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// recordStatuses wraps the handler of New so that the status of each answer is in answered. The
// ServeMux of New sets the request's Pattern, which the wrapper reads after it.
func recordStatuses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		answered.Store(r.Pattern+" "+strconv.Itoa(rec.status()), true)
	})
}

// statusRecorder keeps the status that a handler writes.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// status is the status written, 200 when the handler wrote nothing.
func (s *statusRecorder) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}

// undocumented returns the entries of answered whose status the route's operation in
// openapi.yaml does not list, but for anyOperation. The catch-all /api/, whose pattern has no
// method, is no operation.
func undocumented() ([]string, error) {
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"` // a method's operation, or the path's parameters
	}
	if err := yaml.Unmarshal(openAPI, &doc); err != nil {
		return nil, err
	}
	var missing []string
	var err error
	answered.Range(func(k, _ any) bool {
		key, _ := k.(string)
		i := strings.LastIndexByte(key, ' ')
		pattern, status := key[:i], key[i+1:]
		method, path, ok := strings.Cut(pattern, " ")
		if !ok || slices.Contains(anyOperation, status) {
			return true
		}
		op, ok := doc.Paths[path][strings.ToLower(method)].(map[string]any)
		if !ok {
			err = errors.Join(err, fmt.Errorf("no operation %s, which the server answered", pattern))
			return true
		}
		if responses, _ := op["responses"].(map[string]any); responses[status] == nil {
			missing = append(missing, key)
		}
		return true
	})
	slices.Sort(missing)
	return missing, err
}
