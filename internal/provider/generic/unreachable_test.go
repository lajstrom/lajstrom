package generic

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// refusingAddr returns an address where nothing listens: a connection there is refused.
func refusingAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// resettingAddr returns the address of a listener that reads the head of each request and then
// resets the connection (SO_LINGER 0), as a registry that crashes mid-request does, and counts
// the connections it reset.
func resettingAddr(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var n atomic.Int64
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = http.ReadRequest(bufio.NewReader(c)) // the client now waits for an answer
				_ = c.(*net.TCPConn).SetLinger(0)
				_ = c.Close()
				n.Add(1)
			}()
		}
	}()
	return l.Addr().String(), &n
}

// quietAddr returns the address of a listener that reads the head of each request and then
// either closes the connection without an answer (drop true), as a registry that crashes or
// restarts mid-request does, or keeps it open and never answers, as a registry that hangs does.
func quietAddr(t *testing.T, drop bool) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				if _, err := http.ReadRequest(r); err != nil || drop {
					return
				}
				_, _ = io.Copy(io.Discard, r) // until the client gives up and closes
			}()
		}
	}()
	return l.Addr().String()
}

// timedOut reports whether err says a timeout (net.Error).
func timedOut(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// oneOf returns whether an error wraps one of causes.
func oneOf(causes ...error) func(error) bool {
	return func(err error) bool {
		return slices.ContainsFunc(causes, func(c error) bool { return errors.Is(err, c) })
	}
}

// TestUnreachableRegistry calls every path of a registry that refuses connections, of one that
// resets them (#593), of one that closes them without an answer, and of one that does not answer
// within registries[].timeout (#670), after the retries: each fails with provider.ErrUnavailable,
// with the transport's error as its cause, as a host name that does not resolve does
// (TestUnresolvableRegistry), and a message that does not name the address. The breaker stays
// closed, so that each call meets the registry; each counts as a failure for it.
func TestUnreachableRegistry(t *testing.T) {
	lowerBreaker(t, 1000)
	reset, resets := resettingAddr(t)
	for _, tt := range []struct {
		name    string
		addr    string
		cause   func(error) bool
		timeout time.Duration // registries[].timeout, the wait for the response headers; 0 is the default
	}{
		{name: "refused", addr: refusingAddr(t), cause: oneOf(syscall.ECONNREFUSED)},
		// A write that meets the reset is EPIPE; one after net/http's read met it first, and
		// closed the connection, is net.ErrClosed (Linux, a body sent after the head).
		{name: "reset", addr: reset, cause: oneOf(syscall.ECONNRESET, syscall.EPIPE, net.ErrClosed)},
		// No answer is EOF; a body sent after the close may meet the reset that follows it.
		{name: "closed", addr: quietAddr(t, true), cause: oneOf(io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.EPIPE, net.ErrClosed)},
		{name: "silent", addr: quietAddr(t, false), cause: timedOut, timeout: 50 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const retries = 2
			p, err := New(config.Registry{Name: "down", Type: Type, URL: "http://" + tt.addr, Retries: retries, Timeout: tt.timeout},
				slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			repo, err := p.Repository(ctx, "raw/scratch")
			if err != nil {
				t.Fatal(err)
			}
			d := digest.FromString("blob")
			blob := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: d, Size: 4}
			manifest := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: 4}
			session := provider.Upload{Location: "/v2/raw/scratch/blobs/uploads/1", ID: "1"}
			before := resets.Load()
			paths := []struct {
				name string
				call func() error
			}{
				{"StatBlob (blob HEAD)", func() error { _, err := p.StatBlob(ctx, "raw/scratch", d); return err }},
				{"OpenBlob (blob GET)", func() error { _, err := p.OpenBlob(ctx, "raw/scratch", d, "", true); return err }},
				{"Exists of a blob", func() error { _, err := repo.Exists(ctx, blob); return err }},
				{"Exists of a manifest (manifest HEAD)", func() error { _, err := repo.Exists(ctx, manifest); return err }},
				{"StatManifest", func() error { _, err := p.StatManifest(ctx, "raw/scratch", d); return err }},
				{"Resolve", func() error { _, err := repo.Resolve(ctx, "v1"); return err }},
				{"Fetch (manifest GET)", func() error { _, err := repo.Fetch(ctx, manifest); return err }},
				{"PushReference (manifest PUT)", func() error { return repo.PushReference(ctx, manifest, strings.NewReader("blob"), "v1") }},
				{"Push of a blob", func() error { return repo.Push(ctx, blob, strings.NewReader("blob")) }},
				{"StartBlobUpload", func() error { _, err := repo.StartBlobUpload(ctx); return err }},
				{"MountBlob", func() error { _, _, err := repo.MountBlob(ctx, d, "raw/other"); return err }},
				{"AppendBlobChunk", func() error { _, err := repo.AppendBlobChunk(ctx, session, strings.NewReader("blob"), 4); return err }},
				{"FinishBlobUpload", func() error {
					_, err := repo.FinishBlobUpload(ctx, session, d, strings.NewReader("blob"), 4)
					return err
				}},
				{"BlobUploadStatus", func() error { _, err := repo.BlobUploadStatus(ctx, session); return err }},
				{"CancelBlobUpload", func() error { return repo.CancelBlobUpload(ctx, session) }},
				{"OpenBlobWriter", func() error {
					w, err := repo.OpenBlobWriter(ctx)
					if err != nil {
						return err
					}
					if _, err := w.Write([]byte("blob")); err != nil {
						return err
					}
					_, err = w.Commit(ctx, d)
					return err
				}},
				{"Tags (tag list)", func() error { return repo.Tags(ctx, "", func([]string) error { return nil }) }},
				{"Delete of a manifest", func() error { return repo.Delete(ctx, manifest) }},
				{"ListRepositories", func() error { _, err := p.ListRepositories(ctx, "raw/"); return err }},
				{"Ping", func() error { return p.Ping(ctx) }},
				{"Capabilities", func() error { _, err := p.Capabilities(ctx); return err }},
			}
			for _, pt := range paths {
				err := pt.call()
				switch {
				case !errors.Is(err, provider.ErrUnavailable) || !tt.cause(err):
					t.Errorf("%s: %v, want provider.ErrUnavailable with the error of the %s registry as its cause", pt.name, err, tt.name)
				case strings.Contains(err.Error(), tt.addr):
					t.Errorf("%s: the message %q names the address", pt.name, err)
				}
				if _, ok := provider.RetryAfter(err); ok {
					t.Errorf("%s: %v says a time to wait, want none", pt.name, err)
				}
			}
			b := p.clients.breaker
			b.mu.Lock()
			failures := b.failures
			b.mu.Unlock()
			if failures < len(paths) {
				t.Errorf("the breaker counted %d failures for %d paths, want one for each at least", failures, len(paths))
			}
			if tt.name == "reset" {
				// A HEAD is retried: StatBlob alone cost 1+retries connections.
				if n := resets.Load() - before; n < int64(len(paths)+retries) {
					t.Errorf("the registry reset %d connections for %d paths, want the retries too", n, len(paths))
				}
			}
		})
	}
}
