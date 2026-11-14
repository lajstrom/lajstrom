//go:build linux || darwin

package postgres

import (
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

// TestServerSent peeks at the client end of a TCP connection on loopback, and of TLS over it:
// silent while the server sends nothing; sent once it wrote a byte, which stays there for the
// next read; sent once it closed the connection. A connection that is no socket is silent.
func TestServerSent(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	pair := func(t *testing.T) (client, server net.Conn) {
		t.Helper()
		client, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		server, err = l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
		return client, server
	}
	// eventually waits up to a second for what the server did to reach the client.
	eventually := func(c net.Conn) bool {
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if serverSent(c) {
				return true
			}
		}
		return false
	}

	for _, tt := range []struct {
		name string
		wrap func(net.Conn) net.Conn
	}{
		{"tcp", func(c net.Conn) net.Conn { return c }},
		{"tls", func(c net.Conn) net.Conn { return tls.Client(c, &tls.Config{ServerName: "localhost"}) }},
	} {
		t.Run(tt.name+" silent", func(t *testing.T) {
			client, _ := pair(t)
			time.Sleep(20 * time.Millisecond)
			if serverSent(tt.wrap(client)) {
				t.Error("serverSent = true on a connection the server neither wrote to nor closed")
			}
		})
		t.Run(tt.name+" wrote", func(t *testing.T) {
			client, server := pair(t)
			if _, err := server.Write([]byte("E")); err != nil {
				t.Fatal(err)
			}
			if !eventually(tt.wrap(client)) {
				t.Fatal("serverSent = false after the server wrote a byte")
			}
			b := make([]byte, 1)
			if _, err := io.ReadFull(client, b); err != nil || b[0] != 'E' {
				t.Errorf("the byte after the peek: %q, %v; want it unread, E", b, err)
			}
		})
		t.Run(tt.name+" closed", func(t *testing.T) {
			client, server := pair(t)
			_ = server.Close()
			if !eventually(tt.wrap(client)) {
				t.Error("serverSent = false after the server closed the connection")
			}
		})
	}

	t.Run("no socket", func(t *testing.T) {
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
		if serverSent(client) {
			t.Error("serverSent = true on a net.Pipe")
		}
	})
}
