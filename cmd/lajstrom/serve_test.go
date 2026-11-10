package main

import (
	"net"
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
