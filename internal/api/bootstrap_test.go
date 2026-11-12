package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// bootstrapFixture returns the API with a bootstrap whose token, valid, is the one returned.
func bootstrapFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t, Options{})
	token := strings.Repeat("t", auth.MinBootstrapTokenLen)
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	boot := &auth.Bootstrap{Index: f.st, Users: &auth.Users{Index: f.st, Now: f.repos.Now}, TTL: time.Hour}
	if err := boot.Arm(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	f.h = recordStatuses(New(Options{Repos: f.repos, Bootstrap: boot}))
	return f, token
}

// bootstrapBody is a BootstrapRequest as JSON.
func bootstrapBody(token, name, password string) string {
	return `{"token":"` + token + `","name":"` + name + `","password":"` + password + `"}`
}

// TestBootstrapEndpoint: a wrong token is 403, a bad body or password 400 that leaves the token
// valid, the token 201 with the admin, who may manage the users; then every request is the 404
// of a path that is no endpoint.
func TestBootstrapEndpoint(t *testing.T) {
	f, token := bootstrapFixture(t)
	anon := auth.Principal{}
	wantError(t, f.do("POST", "/api/v1/bootstrap", bootstrapBody(token+"x", "root", "root-password"), anon),
		http.StatusForbidden, codeForbidden, "the bootstrap token is refused")
	wantError(t, f.do("POST", "/api/v1/bootstrap", `{"token":"`+token+`"`, anon), http.StatusBadRequest, codeInvalid)
	wantError(t, f.do("POST", "/api/v1/bootstrap", bootstrapBody(token, "root", "short"), anon),
		http.StatusBadRequest, codeInvalid, "password")

	rr := f.do("POST", "/api/v1/bootstrap", bootstrapBody(token, "root", "root-password"), anon)
	var u auth.User
	decodeJSON(t, rr, http.StatusCreated, &u)
	if u.Name != "root" || rr.Header().Get("Location") != "/api/v1/users/root" || strings.Contains(rr.Body.String(), "argon2") {
		t.Errorf("201 %s with Location %q", rr.Body, rr.Header().Get("Location"))
	}
	events, err := f.st.ListAudit(t.Context(), index.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != auth.ActionUserCreate || !strings.Contains(string(events[0].Details), `"source":"bootstrap"`) {
		t.Errorf("audit %+v, want one user.create with the source bootstrap", events)
	}

	for _, body := range []string{bootstrapBody(token, "root2", "root-password"), "not json"} {
		wantError(t, f.do("POST", "/api/v1/bootstrap", body, anon), http.StatusNotFound, codeNotFound,
			"POST /api/v1/bootstrap is not an endpoint of this version")
	}
}

// TestBootstrapEndpointThrottled: the BootstrapFailures-th wrong token of an address makes its
// next request 429 with Retry-After, the right token too.
func TestBootstrapEndpointThrottled(t *testing.T) {
	f, token := bootstrapFixture(t)
	send := func(tok string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/bootstrap", strings.NewReader(bootstrapBody(tok, "root", "root-password")))
		r = r.WithContext(auth.WithClientAddress(r.Context(), "203.0.113.9"))
		rr := httptest.NewRecorder()
		f.h.ServeHTTP(rr, r)
		return rr
	}
	for range auth.BootstrapFailures {
		wantStatus(t, send("wrong"), http.StatusForbidden)
	}
	rr := send(token)
	wantError(t, rr, http.StatusTooManyRequests, codeUnavailable, "too many wrong bootstrap tokens")
	if rr.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}

// TestBootstrapEndpointOff: without a bootstrap the path is no endpoint.
func TestBootstrapEndpointOff(t *testing.T) {
	f := newFixture(t, Options{})
	wantError(t, f.do("POST", "/api/v1/bootstrap", bootstrapBody("t", "root", "root-password"), auth.Principal{}),
		http.StatusNotFound, codeNotFound)
}
