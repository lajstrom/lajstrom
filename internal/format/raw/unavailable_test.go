package raw

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestRegistryUnavailable uploads a file while the circuit breaker of the registry is open: the
// real generic provider over a registry that answers every request with 503, after enough
// failed pings to open it. The frontend answers 503 with Retry-After, the seconds left until
// the breaker's trial request (at most its 30 s cooldown), in its plain-text envelope.
func TestRegistryUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	reg, err := generic.New(config.Registry{Name: "local", Type: generic.Type, URL: srv.URL, Retries: -1}, slog.New(slog.DiscardHandler))
	must(t, err)
	for range 5 {
		_ = reg.Ping(t.Context())
	}
	e := newEnv(t, envOptions{registry: reg})

	rr := e.do(http.MethodPut, target("dir/file.txt"), strings.NewReader("content"))
	if rr.Code != http.StatusServiceUnavailable || rr.Body.String() != "Service Unavailable\n" {
		t.Errorf("PUT while the breaker is open = %d %q, want 503 Service Unavailable", rr.Code, rr.Body)
	}
	if s, err := strconv.Atoi(rr.Header().Get("Retry-After")); err != nil || s < 1 || s > 30 {
		t.Errorf("Retry-After %q, want 1 to 30 seconds", rr.Header().Get("Retry-After"))
	}
}
