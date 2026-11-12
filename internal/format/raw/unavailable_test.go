package raw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
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

// TestIndexUnavailableAtConfirm uploads a file whose index write fails after the registry took
// its manifest, the database out (#641): 503 with Retry-After: 5 and the status text only, not
// a 500 nor the database's error; the claim is released, and the same PUT again is 201.
func TestIndexUnavailableAtConfirm(t *testing.T) {
	e := newEnv(t, envOptions{ingested: downAtConfirm})
	rr := e.do(http.MethodPut, target("dir/file.txt"), strings.NewReader("content"))
	if rr.Code != http.StatusServiceUnavailable || rr.Body.String() != "Service Unavailable\n" || rr.Header().Get("Retry-After") != "5" {
		t.Fatalf("PUT while the index is out = %d %q, Retry-After %q; want 503 Service Unavailable, 5",
			rr.Code, rr.Body, rr.Header().Get("Retry-After"))
	}
	if len(e.prov.PushReferences()) != 1 || len(e.versions("dir/file.txt")) != 0 {
		t.Fatalf("%d manifest pushes, %d versions; want the push and no version", len(e.prov.PushReferences()), len(e.versions("dir/file.txt")))
	}
	pkgPath, err := Plugin{}.EscapeName("dir/file.txt")
	must(t, err)
	tag := profile.Tag(digest.FromString("content").Encoded()[:versionLen], "")
	if c, err := e.st.GetTagClaim(t.Context(), e.repo.ID, pkgPath, tag); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the claim on the tag %s: %+v, %v; want it released", tag, c, err)
	}
	e.put("dir/file.txt", "content")
	if vs := e.versions("dir/file.txt"); len(vs) != 1 {
		t.Errorf("versions after the same PUT again: %+v, want one", vs)
	}
}

// downAtConfirm wraps st: the first transaction that writes a package, the confirm of a publish
// after the registry took its manifest, fails as a database that dropped out does, with
// index.ErrUnavailable and a cause that names its host.
func downAtConfirm(st index.Store) index.Store { return &confirmDown{Store: st} }

type confirmDown struct {
	index.Store
	done atomic.Bool
}

func (s *confirmDown) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.Store.Tx(ctx, func(w index.Writer) error { return fn(confirmDownWriter{Writer: w, s: s}) })
}

type confirmDownWriter struct {
	index.Writer
	s *confirmDown
}

func (w confirmDownWriter) UpsertPackage(ctx context.Context, p *index.Package) error {
	if w.s.done.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: packages: dial tcp 10.0.0.7:5432: connect: connection refused", index.ErrUnavailable)
	}
	return w.Writer.UpsertPackage(ctx, p)
}

// TestIndexUnavailableAtDelete deletes a file whose index write fails after the registry deleted
// its manifest, the database out (#668): 503 with Retry-After: 5 and the status text only, not a
// 500; the version stays in the index. The same DELETE again finds the manifest gone and deletes
// the version: 204, and the file is 404.
func TestIndexUnavailableAtDelete(t *testing.T) {
	e := newEnv(t, envOptions{ingested: downAtDelete})
	v := e.put(fileTxt, "content")
	rr := e.do(http.MethodDelete, target(fileTxt), nil, principalHeader, "alice")
	if rr.Code != http.StatusServiceUnavailable || rr.Body.String() != "Service Unavailable\n" || rr.Header().Get("Retry-After") != "5" {
		t.Fatalf("DELETE while the index is out = %d %q, Retry-After %q; want 503 Service Unavailable, 5",
			rr.Code, rr.Body, rr.Header().Get("Retry-After"))
	}
	if e.manifest("raw/scratch/docs/file.txt", digest.Digest(v.Digest)) != nil || len(e.versions(fileTxt)) != 1 {
		t.Fatalf("after the 503: the registry has the manifest, or the index has %d versions; want it gone and one version", len(e.versions(fileTxt)))
	}
	if rr := e.do(http.MethodDelete, target(fileTxt), nil, principalHeader, "alice"); rr.Code != http.StatusNoContent {
		t.Fatalf("the same DELETE again = %d %s, want 204", rr.Code, rr.Body)
	}
	if rr := e.do(http.MethodGet, target(fileTxt), nil); rr.Code != http.StatusNotFound || len(e.versions(fileTxt)) != 0 {
		t.Errorf("after the retry: GET %d, %d versions; want 404 and none", rr.Code, len(e.versions(fileTxt)))
	}
}

// downAtDelete wraps st: the first transaction that deletes a version, the index write of a
// delete after the registry deleted the manifest, fails as a database that dropped out does,
// with index.ErrUnavailable.
func downAtDelete(st index.Store) index.Store { return &deleteDown{Store: st} }

type deleteDown struct {
	index.Store
	done atomic.Bool
}

func (s *deleteDown) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.Store.Tx(ctx, func(w index.Writer) error { return fn(deleteDownWriter{Writer: w, s: s}) })
}

type deleteDownWriter struct {
	index.Writer
	s *deleteDown
}

func (w deleteDownWriter) DeleteVersion(ctx context.Context, id int64) error {
	if w.s.done.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: versions: dial tcp 10.0.0.7:5432: connect: connection refused", index.ErrUnavailable)
	}
	return w.Writer.DeleteVersion(ctx, id)
}
