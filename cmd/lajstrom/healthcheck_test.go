package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// healthServer answers /-/readyz with ready and its body, /-/healthz with 200 ok, and records
// the paths it was asked.
func healthServer(t *testing.T, readyStatus int, readyBody string) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/-/readyz":
			w.WriteHeader(readyStatus)
			_, _ = w.Write([]byte(readyBody))
		case "/-/healthz":
			_, _ = w.Write([]byte("ok\n"))
		case "/moved":
			_, _ = w.Write([]byte("ok\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), paths...) }
}

// TestHealthcheck runs healthcheck --url against httptest servers: 200 is exit 0 and one line
// on stdout; anything else is an error of one line, which main prints and exits 1 with.
func TestHealthcheck(t *testing.T) {
	ready, _ := healthServer(t, http.StatusOK, "ok\n")
	down, _ := healthServer(t, http.StatusServiceUnavailable, "a registry has not answered for a minute\n")
	noisy, _ := healthServer(t, http.StatusServiceUnavailable, "line one\x1b[31m\nline two\n")
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	tests := []struct {
		name    string
		args    []string
		wantOut string // the whole stdout when the check passes
		wantErr string // substring of the error otherwise
	}{
		{"ready", []string{"--url", ready.URL}, ready.URL + "/-/readyz: 200 ok\n", ""},
		{"ready, trailing slash", []string{"--url", ready.URL + "/"}, ready.URL + "/-/readyz: 200 ok\n", ""},
		{"live", []string{"--url", ready.URL, "--live"}, ready.URL + "/-/healthz: 200 ok\n", ""},
		{"not ready", []string{"--url", down.URL}, "", "unhealthy: " + down.URL + "/-/readyz: 503 a registry has not answered for a minute"},
		{"not ready, but up", []string{"--url", down.URL, "--live"}, down.URL + "/-/healthz: 200 ok\n", ""},
		{"control characters dropped", []string{"--url", noisy.URL}, "", "503 line one[31m"},
		{"nobody listens", []string{"--url", closed.URL}, "", "unhealthy: " + closed.URL + "/-/readyz: dial tcp"},
		{"not a base URL", []string{"--url", ready.URL + "/-/readyz"}, "", "is not a server's base URL"},
		{"not http", []string{"--url", "ftp://127.0.0.1:21"}, "", "is not a server's base URL"},
		{"credentials", []string{"--url", "http://u:p@127.0.0.1:1"}, "", "is not a server's base URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --config names a file that does not exist: with --url, none is read.
			args := append([]string{"healthcheck", "--config", filepath.Join(t.TempDir(), "none.yaml")}, tt.args...)
			out, _, err := run(t, nil, args...)
			if tt.wantErr == "" {
				if err != nil || out != tt.wantOut {
					t.Fatalf("stdout %q, error %v; want %q", out, err, tt.wantOut)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), "\x1b") {
				t.Errorf("the error is not one printable line: %q", err)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing", out)
			}
		})
	}
}

// TestHealthcheckTimeout: a server that does not answer is unhealthy after --timeout, not after
// the server's own time.
func TestHealthcheckTimeout(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	start := time.Now()
	_, _, err := run(t, nil, "healthcheck", "--url", slow.URL, "--timeout", "200ms")
	if err == nil || !strings.Contains(err.Error(), "unhealthy: "+slow.URL+"/-/readyz: no answer within 200ms") {
		t.Fatalf("error = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s", d)
	}
}

// TestHealthcheckRedirect: a redirect is an answer other than 200, not followed.
func TestHealthcheckRedirect(t *testing.T) {
	srv, asked := healthServer(t, http.StatusOK, "ok\n")
	redirect := httptest.NewServer(http.RedirectHandler(srv.URL+"/moved", http.StatusTemporaryRedirect))
	defer redirect.Close()
	_, _, err := run(t, nil, "healthcheck", "--url", redirect.URL)
	if err == nil || !strings.Contains(err.Error(), "/-/readyz: 307") {
		t.Fatalf("error = %v, want a 307", err)
	}
	if got := asked(); len(got) != 0 {
		t.Errorf("the redirect was followed: %v", got)
	}
}

// TestHealthcheckFromConfig: without --url the server is server.listen of the configuration on
// loopback, and LAJSTROM_SERVER_LISTEN overrides it. Nothing else of the configuration or the
// environment is read: a registry whose password variable is unset, which every other command
// refuses, does not matter, and the only variables looked up are LAJSTROM_CONFIG and
// LAJSTROM_SERVER_LISTEN.
func TestHealthcheckFromConfig(t *testing.T) {
	srv, asked := healthServer(t, http.StatusOK, "ok\n")
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	dir := t.TempDir()
	cfg := filepath.Join(dir, "lajstrom.yaml")
	body := "server:\n  listen: \":" + port + "\"\n" +
		"registries:\n  - name: local\n    url: \"http://localhost:5000\"\n    auth: {username: lajstrom, passwordEnv: REGISTRY_PASSWORD}\n" +
		"keys:\n  masterKeyEnv: LAJSTROM_MASTER_KEY\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		looked = map[string]bool{}
	)
	env := map[string]string{"LAJSTROM_MASTER_KEY": strings.Repeat("ab", 32), "LAJSTROM_DATABASE_DSN": "postgres://db/x?password=" + strings.Repeat("s", 8)}
	var stdout, stderr bytes.Buffer
	cmd := newRootCmd(func(k string) (string, bool) {
		mu.Lock()
		looked[k] = true
		mu.Unlock()
		v, ok := env[k]
		return v, ok
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"healthcheck", "--config", cfg})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
	if want := "http://127.0.0.1:" + port + "/-/readyz: 200 ok\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	var names []string
	for k := range looked {
		names = append(names, k)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "LAJSTROM_CONFIG,LAJSTROM_SERVER_LISTEN" {
		t.Errorf("variables looked up: %v", names)
	}
	if got := asked(); len(got) != 1 || got[0] != "/-/readyz" {
		t.Errorf("asked %v", got)
	}

	// LAJSTROM_SERVER_LISTEN wins over the file: a port nobody listens on.
	closed := httptest.NewServer(http.NotFoundHandler())
	closedListen := closed.Listener.Addr().String()
	closed.Close()
	_, _, err := run(t, map[string]string{"LAJSTROM_SERVER_LISTEN": closedListen}, "healthcheck", "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "unhealthy: http://"+closedListen+"/-/readyz") {
		t.Errorf("with LAJSTROM_SERVER_LISTEN: %v", err)
	}

	_, _, err = run(t, nil, "healthcheck", "--config", filepath.Join(dir, "missing.yaml"))
	if err == nil || !strings.Contains(err.Error(), "name the server with --url") {
		t.Errorf("without a configuration: %v", err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("server:\n  listen: \"8080\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = run(t, nil, "healthcheck", "--config", bad); err == nil || !strings.Contains(err.Error(), `server.listen: "8080" is not host:port`) {
		t.Errorf("a bad server.listen: %v", err)
	}
}

func TestListenURL(t *testing.T) {
	for listen, want := range map[string]string{
		":8080":          "http://127.0.0.1:8080",
		"0.0.0.0:8080":   "http://127.0.0.1:8080",
		"[::]:8080":      "http://[::1]:8080",
		"127.0.0.1:9090": "http://127.0.0.1:9090",
		"10.1.2.3:8080":  "http://10.1.2.3:8080",
		"lajstrom:8080":  "http://lajstrom:8080",
	} {
		if got := listenURL(listen); got != want {
			t.Errorf("listenURL(%q) = %q, want %q", listen, got, want)
		}
	}
}
