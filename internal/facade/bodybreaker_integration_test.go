//go:build integration

package facade

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/server"
)

// clientStall is the server.stallTimeout of the client body tests.
const clientStall = 300 * time.Millisecond

// bodyServer is the facade over the registry of LAJSTROM_TEST_REGISTRY, with the generic
// provider and its circuit breaker, behind the server's middleware with a stallTimeout of
// clientStall, served over HTTP/2 with TLS, as when Lajstrom terminates TLS itself, or over
// HTTP/1.1.
type bodyServer struct {
	srv  *httptest.Server
	name string     // oci/images/it-<random>
	logs *logBuffer // the provider's
}

func newBodyServer(t *testing.T, h2 bool) *bodyServer {
	t.Helper()
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	b := &bodyServer{name: "oci/images/it-" + randomHex(t), logs: &logBuffer{}}
	reg, err := generic.New(config.Registry{Name: "local", Type: generic.Type, URL: "http://" + addr, Retries: -1,
		Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth:   config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")}},
		slog.New(slog.NewTextHandler(b.logs, nil)))
	must(t, err)
	e := newEnv(t, envOptions{registry: func(*providertest.Fake) provider.Provider { return reg }})
	s, err := server.New(server.Options{Listen: "127.0.0.1:0", Log: slog.New(slog.DiscardHandler), StallTimeout: clientStall, Facade: e.h})
	must(t, err)
	b.srv = httptest.NewUnstartedServer(s.Handler())
	if h2 {
		b.srv.EnableHTTP2 = true
		b.srv.StartTLS()
	} else {
		b.srv.Start()
	}
	t.Cleanup(b.srv.Close)
	return b
}

// open starts an upload session and returns its Location.
func (b *bodyServer) open(t *testing.T) string {
	t.Helper()
	resp, err := b.srv.Client().Post(b.srv.URL+"/v2/"+b.name+"/blobs/uploads/", "", nil)
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST to open a session: %d", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

// stalledChunk sends a chunk of 1000 bytes to the session at loc over HTTP/2 that stops after
// 300, and returns the answer.
func (b *bodyServer) stalledChunk(t *testing.T, loc string) (int, string) {
	t.Helper()
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, b.srv.URL+loc, pr)
	must(t, err)
	req.ContentLength = 1000
	req.Header.Set("Content-Type", octetStream)
	go func() { _, _ = pw.Write(bytes.Repeat([]byte("s"), 300)) }()
	resp, err := b.srv.Client().Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 2 {
		t.Fatalf("the chunk went over %s, want HTTP/2", resp.Proto)
	}
	return answerCode(t, resp)
}

// rawChunk sends a chunk to the session at loc over HTTP/1.1, the header lines hdr and then
// body, sends nothing more, and returns the answer.
func (b *bodyServer) rawChunk(t *testing.T, loc, hdr, body string) (int, string) {
	t.Helper()
	c, err := net.Dial("tcp", b.srv.Listener.Addr().String())
	must(t, err)
	defer func() { _ = c.Close() }()
	_, err = io.WriteString(c, "PATCH "+loc+" HTTP/1.1\r\nHost: lajstrom\r\nContent-Type: "+octetStream+"\r\n"+hdr+"\r\n"+body)
	must(t, err)
	_ = c.SetReadDeadline(time.Now().Add(10 * clientStall))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	return answerCode(t, resp)
}

// answerCode returns the status of resp and the code of its one error.
func answerCode(t *testing.T, resp *http.Response) (int, string) {
	t.Helper()
	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Errors) != 1 {
		t.Fatalf("answer %d is not one error of the distribution format (%v)", resp.StatusCode, err)
	}
	return resp.StatusCode, body.Errors[0].Code
}

// TestIntegrationClientBodyBreaker sends five chunks in a row whose client fails them, then a
// sixth client's push (#638): a body that fails to read is the client's doing, answered 408 or
// 400, and leaves the registry's circuit breaker closed, so the push goes through. Over HTTP/2,
// where net/http does not cancel a request whose body server.stallTimeout cut, chunks that
// stall; over HTTP/1.1, chunks whose chunked encoding is broken, and chunks that stall, whose
// request net/http cancels. The sessions are opened first, so that no answer of the registry
// comes between the failures.
func TestIntegrationClientBodyBreaker(t *testing.T) {
	pad := strings.Repeat("b", 300)
	tests := []struct {
		name   string
		h2     bool
		send   func(b *bodyServer, t *testing.T, loc string) (int, string)
		status int
	}{
		{"HTTP/2, stalled", true, (*bodyServer).stalledChunk, http.StatusRequestTimeout},
		{"HTTP/1.1, broken chunked encoding", false, func(b *bodyServer, t *testing.T, loc string) (int, string) {
			return b.rawChunk(t, loc, "Transfer-Encoding: chunked\r\n", "12c\r\n"+pad+"\r\nzz\r\n")
		}, http.StatusBadRequest},
		{"HTTP/1.1, stalled", false, func(b *bodyServer, t *testing.T, loc string) (int, string) {
			return b.rawChunk(t, loc, "Content-Length: 1000\r\n", pad)
		}, http.StatusRequestTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newBodyServer(t, tt.h2)
			var sessions []string
			for range 5 {
				sessions = append(sessions, b.open(t))
			}
			for i, loc := range sessions {
				if status, code := tt.send(b, t, loc); status != tt.status || code != codeBlobUploadInvalid {
					t.Errorf("chunk %d: %d %s, want %d %s", i+1, status, code, tt.status, codeBlobUploadInvalid)
				}
			}
			blob := []byte("a well-behaved client's blob " + randomHex(t))
			resp, err := b.srv.Client().Post(b.srv.URL+"/v2/"+b.name+"/blobs/uploads/?digest="+digest.FromBytes(blob).String(),
				octetStream, bytes.NewReader(blob))
			must(t, err)
			msg, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				t.Errorf("the sixth client's push: %d %s, want 201", resp.StatusCode, msg)
			}
			if logs := b.logs.String(); strings.Contains(logs, "circuit open") {
				t.Errorf("the circuit breaker opened:\n%s", logs)
			}
		})
	}
}
