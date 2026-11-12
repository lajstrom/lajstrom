package main

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// bootstrapServer serves the API over a fresh index whose bootstrap token is the one returned,
// and returns its URL.
func bootstrapServer(t *testing.T) (string, string) {
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
	token := strings.Repeat("k", auth.MinBootstrapTokenLen)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	boot := &auth.Bootstrap{Index: st, Users: &auth.Users{Index: st}, TTL: time.Hour}
	if err := boot.Arm(ctx, file); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(api.Options{Repos: &api.Repos{Index: st}, Bootstrap: boot}))
	t.Cleanup(srv.Close)
	return srv.URL, file
}

// TestAdminBootstrap: the token from a file and the password from standard input make the
// admin, printed as JSON; a second run is told that an admin exists; a wrong token and a bad
// --server are refused.
func TestAdminBootstrap(t *testing.T) {
	url, file := bootstrapServer(t)
	wrong := filepath.Join(t.TempDir(), "wrong")
	if err := os.WriteFile(wrong, []byte(strings.Repeat("w", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	args := func(server, token string) []string {
		return []string{"admin", "bootstrap", "--server", server, "--name", "root", "--token-file", token, "--password-stdin"}
	}
	for _, tt := range []struct {
		name, server, token, password, want string
	}{
		{"wrong token", url, wrong, "root-password", "403 the bootstrap token is refused"},
		{"short password", url, file, "short", "400 password: must be at least"},
		{"a path", url + "/api", file, "root-password", "is not a server's base URL"},
		{"no token file", url, filepath.Join(t.TempDir(), "none"), "root-password", "--token-file"},
	} {
		_, _, err := runIn(t, nil, strings.NewReader(tt.password+"\n"), args(tt.server, tt.token)...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
	}

	out, _, err := runIn(t, nil, strings.NewReader("root-password\n"), args(url+"/", file)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, `{"uid":"`) || !strings.Contains(out, `"name":"root"`) || strings.Count(out, "\n") != 1 {
		t.Errorf("stdout %q, want the admin as JSON on one line", out)
	}
	_, _, err = runIn(t, nil, strings.NewReader("root-password\n"), args(url, file)...)
	if err == nil || !strings.Contains(err.Error(), "404 an admin exists already") {
		t.Errorf("a second bootstrap: %v, want the 404 that an admin exists", err)
	}
}

// TestAdminBootstrapPrompts: without --token-file the token and the password are prompted
// for; --password-stdin without it is refused, standard input cannot hold both.
func TestAdminBootstrapPrompts(t *testing.T) {
	url, file := bootstrapServer(t)
	b, err := os.ReadFile(file) // #nosec G304 -- the test's own file
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = runIn(t, nil, strings.NewReader("x"), "admin", "bootstrap", "--server", url, "--name", "root", "--password-stdin")
	if err == nil || !strings.Contains(err.Error(), "--password-stdin needs --token-file") {
		t.Errorf("--password-stdin without --token-file: %v", err)
	}
	fakeTerminal(t, nil, strings.TrimSpace(string(b)), "root-password", "root-password")
	out, stderr, err := runIn(t, nil, ttyFile(t), "admin", "bootstrap", "--server", url, "--name", "root")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"name":"root"`) || !strings.Contains(stderr, "Bootstrap token: ") {
		t.Errorf("stdout %q, stderr %q", out, stderr)
	}
}

// TestAdminBootstrapServerOfConfig: without --server the request goes to server.listen of the
// configuration, on loopback.
func TestAdminBootstrapServerOfConfig(t *testing.T) {
	url, file := bootstrapServer(t)
	cfg := filepath.Join(t.TempDir(), "lajstrom.yaml")
	listen := strings.TrimPrefix(url, "http://")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, "server:\n  listen: %q\n", listen), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runIn(t, nil, strings.NewReader("root-password"), "admin", "bootstrap", "--config", cfg, "--name", "root",
		"--token-file", file, "--password-stdin")
	if err != nil || !strings.Contains(out, `"name":"root"`) {
		t.Errorf("admin bootstrap with the server of the configuration: %q, %v", out, err)
	}
}

// TestAdminBootstrapExitStatus: a configuration that cannot be read or is invalid exits 2, as
// for every command but healthcheck (docs/configuration.md); a server that refuses or cannot
// be reached exits 1.
func TestAdminBootstrapExitStatus(t *testing.T) {
	url, file := bootstrapServer(t)
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	wrong := write("wrong", strings.Repeat("w", 40))
	down := httptest.NewServer(nil)
	down.Close()
	for _, tt := range []struct {
		name   string
		args   []string
		want   string
		status int
	}{
		{"a missing configuration", []string{"--config", filepath.Join(dir, "missing.yaml"), "--token-file", file},
			"name the server with --server, or the configuration with --config", 2},
		{"server.listen that is not host:port", []string{"--config", write("listen.yaml", "server:\n  listen: \"8080\"\n"), "--token-file", file},
			`server.listen: "8080" is not host:port`, 2},
		{"an unknown key", []string{"--config", write("typo.yaml", "server:\n  lisen: \":8080\"\n"), "--token-file", file},
			"lisen", 2},
		{"a refused token", []string{"--server", url, "--token-file", wrong}, "403 the bootstrap token is refused", 1},
		{"a server that is down", []string{"--server", down.URL, "--token-file", file}, "bootstrap: ", 1},
	} {
		args := append([]string{"admin", "bootstrap", "--name", "root", "--password-stdin"}, tt.args...)
		_, _, err := runIn(t, nil, strings.NewReader("root-password\n"), args...)
		if err == nil || !strings.Contains(err.Error(), tt.want) || exitStatus(err) != tt.status {
			t.Errorf("%s: %v (exit %d), want %q, exit %d", tt.name, err, exitStatus(err), tt.want, tt.status)
		}
	}
}
