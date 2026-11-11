package format_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestServeBlobDigest serves blobs over real connections from a fake registry that answers
// bytes other than the blob's: the client gets a short body and then an error, the connection
// closed (HTTP/1, with Content-Length or chunked) or the stream reset (HTTP/2), never the whole
// body; the abort is counted, by registry, and its panic says why. The right bytes go out
// whole, and a range (206) is not hashed (docs/spec/guarantees.md, INV-40).
func TestServeBlobDigest(t *testing.T) {
	const size = 1 << 20
	right := bytes.Repeat([]byte("lajstrom"), size/8)
	wrong := bytes.Clone(right)
	wrong[size-1] ^= 1 // the last byte only: all the others go out
	d := digest.FromBytes(right)
	tests := []struct {
		name    string
		body    []byte
		size    int64  // the Size of the backend's answer; -1: unknown, and the answer is chunked
		status  int    // the backend's status; 0 is 200
		rng     string // the client's Range
		http2   bool
		aborted bool
	}{
		{name: "wrong bytes with Content-Length", body: wrong, size: size, aborted: true},
		{name: "wrong bytes chunked", body: wrong, size: -1, aborted: true},
		{name: "wrong bytes over HTTP/2", body: wrong, size: size, http2: true, aborted: true},
		{name: "a small blob of wrong bytes", body: []byte("not the blob"), size: 12, aborted: true},
		{name: "the right bytes", body: right, size: size},
		{name: "the right bytes chunked", body: right, size: -1},
		{name: "the right bytes over HTTP/2", body: right, size: size, http2: true},
		{name: "a range is not hashed", body: wrong[size-10:], size: 10, status: http.StatusPartialContent, rng: "bytes=-10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &blobProvider{Fake: &providertest.Fake{}, open: func(context.Context, string, digest.Digest, string, bool) (provider.Blob, error) {
				return provider.Blob{Status: tt.status, Body: io.NopCloser(bytes.NewReader(tt.body)), Size: tt.size, Digest: d}, nil
			}}
			mismatches := func() float64 { return testutil.ToFloat64(metrics.DownloadDigestMismatches.WithLabelValues("fake")) }
			before := mismatches()
			srv, outcome := blobServer(t, p, d, tt.http2)
			proto, got, err := download(t, srv, tt.rng)
			if tt.http2 && proto != 0 && proto != 2 {
				t.Errorf("protocol HTTP/%d, want HTTP/2", proto)
			}
			if !tt.aborted {
				if err != nil || !bytes.Equal(got, tt.body) || mismatches() != before {
					t.Errorf("got %d bytes, %v, %v more mismatches; want the %d bytes whole", len(got), err, mismatches()-before, len(tt.body))
				}
				wantReturned(t, outcome, nil)
				return
			}
			if err == nil || len(got) >= len(tt.body) {
				t.Errorf("got %d bytes of %d, %v; want a short body and an error", len(got), len(tt.body), err)
			}
			if len(tt.body) == size && (proto == 0 || len(got) == 0) {
				t.Errorf("the response did not start (%v): want a short body before the reset", err)
			}
			wantAbort(t, outcome, "do not have the digest")
			if n := mismatches() - before; n != 1 {
				t.Errorf("%v more mismatches, want 1", n)
			}
		})
	}
}

// download sends GET with the Range rng ("" for none) to srv and returns the major version of
// the response's protocol (0 when no response came), the body it got and the error that ended
// it, nil for a whole body.
func download(t *testing.T, srv *httptest.Server, rng string) (int, []byte, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/raw/scratch/tool", nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.ProtoMajor, body, err
}

// blobServer serves ServeBlob of the blob d from p over HTTP/1, or HTTP/2 over TLS. How each
// ServeBlob ended goes to the channel: the error it returned, nil included, or its panic, which
// goes on, as the server's recovery does, to net/http as http.ErrAbortHandler.
func blobServer(t *testing.T, p provider.Provider, d digest.Digest, http2 bool) (*httptest.Server, chan any) {
	t.Helper()
	outcome := make(chan any, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				select {
				case outcome <- v:
				default:
				}
				panic(http.ErrAbortHandler)
			}
		}()
		err := format.ServeBlob(w, r, p, blobName, d, format.ServeBlobOptions{ContentType: "application/octet-stream"})
		select {
		case outcome <- err:
		default:
		}
	}))
	srv.EnableHTTP2 = http2
	if http2 {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, outcome
}

// served returns how ServeBlob ended in a handler of blobServer (nil: it returned nil).
func served(t *testing.T, outcome chan any) any {
	t.Helper()
	select {
	case v := <-outcome:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("ServeBlob did not end")
		return nil
	}
}

// wantReturned fails t unless ServeBlob returned an error that wraps want, or nil for nil.
func wantReturned(t *testing.T, outcome chan any, want error) {
	t.Helper()
	v := served(t, outcome)
	err, ok := v.(error)
	if v != nil && (!ok || errors.Is(err, http.ErrAbortHandler)) || !errors.Is(err, want) {
		t.Errorf("ServeBlob ended with %v, want %v returned", v, want)
	}
}

// wantAbort fails t unless ServeBlob panicked with an error that wraps http.ErrAbortHandler
// and says why.
func wantAbort(t *testing.T, outcome chan any, why string) {
	t.Helper()
	v := served(t, outcome)
	err, _ := v.(error)
	if !errors.Is(err, http.ErrAbortHandler) || !strings.Contains(fmt.Sprint(v), why) {
		t.Errorf("ServeBlob ended with %v, want a panic wrapping http.ErrAbortHandler that says %q", v, why)
	}
}

// TestServeBlobBrokenStream serves a blob from a fake registry that drops the stream halfway,
// over real connections (#568). Of unknown size, it went out chunked (HTTP/1) or without
// content-length (HTTP/2): ServeBlob aborts the response, and the client gets half the bytes
// and an error, the connection closed or the stream reset, not a clean end that it would take
// for the whole blob. With Content-Length, ServeBlob returns the error and net/http closes the
// connection; the client sees the short body by the length.
func TestServeBlobBrokenStream(t *testing.T) {
	const size = 1 << 20
	whole := bytes.Repeat([]byte("lajstrom"), size/8)
	d := digest.FromBytes(whole)
	for _, tt := range []struct {
		name    string
		size    int64 // the Size of the backend's answer; -1: unknown
		http2   bool
		aborted bool
	}{
		{name: "chunked", size: -1, aborted: true},
		{name: "unknown size over HTTP/2", size: -1, http2: true, aborted: true},
		{name: "with Content-Length", size: size},
		{name: "with Content-Length over HTTP/2", size: size, http2: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &blobProvider{Fake: &providertest.Fake{}, open: func(context.Context, string, digest.Digest, string, bool) (provider.Blob, error) {
				dropped := io.MultiReader(bytes.NewReader(whole[:size/2]), &brokenBody{err: io.ErrUnexpectedEOF})
				return provider.Blob{Body: io.NopCloser(dropped), Size: tt.size, Digest: d}, nil
			}}
			srv, outcome := blobServer(t, p, d, tt.http2)
			proto, got, err := download(t, srv, "")
			if want := map[bool]int{false: 1, true: 2}[tt.http2]; proto != want {
				t.Errorf("protocol HTTP/%d, want HTTP/%d", proto, want)
			}
			if err == nil || len(got) == 0 || len(got) > size/2 {
				t.Errorf("got %d bytes, %v; want at most %d and an error, not a clean end", len(got), err, size/2)
			}
			if tt.aborted {
				wantAbort(t, outcome, "no Content-Length, the response is aborted")
				return
			}
			wantReturned(t, outcome, io.ErrUnexpectedEOF)
		})
	}
}
