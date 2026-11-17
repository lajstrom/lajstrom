package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// passwordServer serves the API over a fresh index with the user alice, whose password
// "alice-password-1" she must change, and returns its URL and the index.
func passwordServer(t *testing.T) (string, index.Store) {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:"+filepath.Join(t.TempDir(), "lajstrom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	users := &auth.Users{Index: st, PasswordPolicy: auth.PasswordPolicy{MinLength: 12, ChangeAtFirstUse: true}}
	if _, err := users.Create(ctx, "alice", "alice-password-1", false, auth.Origin{Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(api.Options{Repos: &api.Repos{Index: st}, Passwords: auth.NewAuthenticator(auth.Options{Index: st,
		Users: users})}))
	t.Cleanup(srv.Close)
	return srv.URL, st
}

// TestChangePassword: change-password reads the current password and the new one from the lines
// of standard input, or from prompts on a terminal, the new one twice; the server's refusals are
// errors with their status and message, and a change clears mustChange (docs/auth.md, Password
// rules, your own password, change at first use).
func TestChangePassword(t *testing.T) {
	url, st := passwordServer(t)
	change := func(stdin string, args ...string) (string, error) {
		_, stderr, err := runIn(t, nil, strings.NewReader(stdin), append([]string{"change-password", "alice", "--server", url}, args...)...)
		return stderr, err
	}
	for _, tt := range []struct{ name, stdin, want string }{
		{"a wrong password", "wrong-password\nalice-password-2\n", "401 the user name or the password is wrong"},
		{"a rule", "alice-password-1\nshort-pw-2\n", "400 password: must be at least 12 characters"},
		{"the current one", "alice-password-1\nalice-password-1\n", "400 password: was used before"},
		{"one line", "alice-password-1\n", "standard input: want the current password on the first line"},
		{"three lines", "alice-password-1\nalice-password-2\nmore\n", "and nothing else"},
		{"an empty line", "\nalice-password-2\n", "a line is empty"},
	} {
		if _, err := change(tt.stdin); err == nil || !strings.Contains(err.Error(), tt.want) || exitStatus(err) != 1 {
			t.Errorf("%s: %v (exit %d), want exit 1 with %q", tt.name, err, exitStatus(err), tt.want)
		}
	}
	if _, _, err := runIn(t, nil, strings.NewReader("a\nb\n"), "change-password", "Alice", "--server", url); err == nil {
		t.Error("a malformed name was taken")
	}
	stderr, err := change("alice-password-1\r\nalice-password-2\r\n")
	if err != nil || !strings.Contains(stderr, "The password of alice is changed.") {
		t.Fatalf("change-password = %v, stderr %q", err, stderr)
	}
	if u, err := st.GetUser(t.Context(), "alice"); err != nil || u.MustChange || len(u.PasswordHistory) != 1 {
		t.Errorf("alice after her change: %+v, %v; want no mustChange and one hash in the history", u, err)
	}
}

// TestChangePasswordPrompts: on a terminal change-password prompts for the current password and
// the new one, twice, and refuses two new ones that differ before it sends anything.
func TestChangePasswordPrompts(t *testing.T) {
	url, st := passwordServer(t)
	fakeTerminal(t, nil, "alice-password-1", "alice-password-3", "alice-password-3")
	_, stderr, err := runIn(t, nil, ttyFile(t), "change-password", "alice", "--server", url)
	if err != nil || !strings.Contains(stderr, "Current password: ") || !strings.Contains(stderr, "Repeat the new password: ") {
		t.Errorf("change-password on a terminal = %v, stderr %q", err, stderr)
	}
	if u, err := st.GetUser(t.Context(), "alice"); err != nil || u.MustChange {
		t.Errorf("alice after her change on a terminal: %+v, %v; want no mustChange", u, err)
	}
	fakeTerminal(t, nil, "alice-password-3", "alice-password-4", "alice-password-5")
	if _, _, err := runIn(t, nil, ttyFile(t), "change-password", "alice", "--server", url); err == nil ||
		!strings.Contains(err.Error(), "the new passwords do not match") {
		t.Errorf("two new passwords that differ: %v", err)
	}
}

// TestChangePasswordThrottled: a 429 says when to try again.
func TestChangePasswordThrottled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"UNAVAILABLE","message":"too many failed logins: try again after Retry-After seconds"}}`))
	}))
	t.Cleanup(srv.Close)
	_, _, err := runIn(t, nil, strings.NewReader("a\nb\n"), "change-password", "alice", "--server", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "429 too many failed logins: try again after Retry-After seconds (Retry-After: 42s)") {
		t.Errorf("a 429: %v", err)
	}
}
