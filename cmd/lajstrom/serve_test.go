package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServeCannotListen checks that serve returns the error of a listener that cannot bind, at
// once: the background work it started before it listens (the expiry of the sessions, the sweep
// of the proxy repos) ends with it. Before #372 serve waited for that work for ever instead,
// and printed nothing after "format mounted".
func TestServeCannotListen(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	addr := taken.Addr().String()
	cfg := writeConfig(t, t.TempDir())
	done := make(chan error, 1)
	go func() {
		_, _, err := run(t, map[string]string{"LAJSTROM_SERVER_LISTEN": addr}, "serve", "--config", cfg)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "listen tcp "+addr) {
			t.Fatalf("serve: %v, want the error of the listener on %s", err, addr)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("serve has not returned 20 s after its listener on %s failed", addr)
	}
}

// TestServeBootstrapTokenFile checks that serve reads auth.bootstrap.tokenFile at start while no
// admin exists, and stops before it listens on a token it refuses.
func TestServeBootstrapTokenFile(t *testing.T) {
	dir := t.TempDir()
	cfg := writeConfig(t, dir)
	short := filepath.Join(dir, "token")
	if err := os.WriteFile(short, []byte("too-short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := run(t, map[string]string{"LAJSTROM_BOOTSTRAP_TOKEN_FILE": short}, "serve", "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "auth.bootstrap.tokenFile "+short+": the token must be 32 to 256 bytes, it has 9") {
		t.Fatalf("serve: %v, want the refusal of the token file", err)
	}
}

// TestServeWarnsPlainHTTPBeyondLoopback checks that serve warns at start when server.insecureHTTP
// is on and server.listen is not a loopback address (#746): Basic credentials would cross the
// network in clear text. It cannot refuse: a container listens on :8080 behind a port published
// on loopback. A bootstrap token it refuses stops serve before it listens.
func TestServeWarnsPlainHTTPBeyondLoopback(t *testing.T) {
	const warning = "level=WARN msg=\"server.insecureHTTP serves plain HTTP beyond loopback"
	for _, tt := range []struct {
		listen string
		warn   bool
	}{
		{":8080", true},
		{"0.0.0.0:8080", true},
		{"127.0.0.1:8080", false},
		{"[::1]:8080", false},
		{"localhost:8080", false},
	} {
		t.Run(tt.listen, func(t *testing.T) {
			dir := t.TempDir()
			cfg := filepath.Join(dir, "lajstrom.yaml")
			body := "server:\n  listen: \"" + tt.listen + "\"\n  insecureHTTP: true\ndatabase:\n  driver: sqlite\n  dsn: \"file:" +
				filepath.Join(dir, "lajstrom.db") + "\"\nauth:\n  bootstrap:\n    tokenFile: " + filepath.Join(dir, "token") + "\n"
			if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "token"), []byte("too-short\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, stderr, err := run(t, nil, "serve", "--config", cfg)
			if err == nil || !strings.Contains(err.Error(), "auth.bootstrap.tokenFile") {
				t.Fatalf("serve: %v, want the refusal of the token file", err)
			}
			if got := strings.Contains(stderr, warning); got != tt.warn {
				t.Errorf("warning logged = %v, want %v; stderr:\n%s", got, tt.warn, stderr)
			}
			if tt.warn && !strings.Contains(stderr, "server.listen="+tt.listen) {
				t.Errorf("the warning does not name server.listen=%s; stderr:\n%s", tt.listen, stderr)
			}
		})
	}
}
