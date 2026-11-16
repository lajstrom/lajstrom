package format_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/blobcrypt"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// encryptedBlob puts the ciphertext of plain into a fake registry that redirects every blob, and
// returns it, the ciphertext's digest and the key that opens it.
func encryptedBlob(t *testing.T, plain string) (*providertest.Fake, digest.Digest, *format.BlobKey) {
	t.Helper()
	opts := blobcrypt.NewPrivate()
	opts.Digest = digest.FromString(plain)
	ct, mac, err := blobcrypt.Seal(opts, []byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	f, d := newBlobFake(t, string(ct))
	f.RedirectURL = func(_ string, d digest.Digest) (string, bool) { return "https://objects.test/" + d.Encoded(), true }
	return f, d, &format.BlobKey{Private: opts, HMAC: mac}
}

// TestServeBlobDecrypt: an encrypted layer is decrypted as it streams, whole or a range of any
// offset, never redirected though the registry and the policy would, and HEAD keeps its length.
func TestServeBlobDecrypt(t *testing.T) {
	plain := strings.Repeat("the plaintext of an encrypted layer, ", 100)
	f, d, key := encryptedBlob(t, plain)
	opts := format.ServeBlobOptions{ContentType: "application/octet-stream", Redirect: true, Decrypt: key}

	rr, err := serveBlob(t, f, d, opts, http.MethodGet)
	if err != nil || rr.Code != http.StatusOK || rr.Body.String() != plain {
		t.Fatalf("GET = %d, %d bytes, %v; want the plaintext", rr.Code, rr.Body.Len(), err)
	}
	wantHeaders(t, "whole", rr, map[string]string{"Location": "", "Content-Length": fmt.Sprint(len(plain)), "ETag": `"` + d.String() + `"`})
	for _, r := range []struct {
		header string
		from   int
		to     int // exclusive
	}{{"bytes=0-9", 0, 10}, {"bytes=17-40", 17, 41}, {"bytes=1000-", 1000, len(plain)}, {"bytes=-5", len(plain) - 5, len(plain)}} {
		rr, err := serveBlob(t, f, d, opts, http.MethodGet, "Range", r.header)
		if err != nil || rr.Code != http.StatusPartialContent || rr.Body.String() != plain[r.from:r.to] {
			t.Errorf("Range %s = %d %q, %v; want %q", r.header, rr.Code, rr.Body, err, plain[r.from:r.to])
		}
	}
	rr, err = serveBlob(t, f, d, opts, http.MethodHead)
	if err != nil || rr.Code != http.StatusOK || rr.Header().Get("Content-Length") != fmt.Sprint(len(plain)) {
		t.Errorf("HEAD = %d %v %v", rr.Code, rr.Header(), err)
	}
}

// TestServeBlobDecryptAborts: a ciphertext changed in the registry, or another blob's key, never
// gives a client the whole plaintext: the response is aborted before its last byte.
func TestServeBlobDecryptAborts(t *testing.T) {
	plain := strings.Repeat("x", 70_000)
	f, d, key := encryptedBlob(t, plain)
	_, _, other := encryptedBlob(t, plain)
	changed := &blobProvider{Fake: f, open: func(ctx context.Context, name string, d digest.Digest, rng string, redirect bool) (provider.Blob, error) {
		b, err := f.OpenBlob(ctx, name, d, rng, redirect)
		if err == nil {
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(b.Body)
			_ = b.Body.Close()
			ct := buf.Bytes()
			ct[len(ct)-1] ^= 1
			b.Body = closerOf(bytes.NewReader(ct))
		}
		return b, err
	}}
	for name, c := range map[string]struct {
		p   provider.Provider
		key *format.BlobKey
	}{"changed ciphertext": {changed, key}, "another key": {f, other}} {
		req := httptest.NewRequest(http.MethodGet, "/raw/scratch/tool", nil)
		rr := httptest.NewRecorder()
		var v any
		func() {
			defer func() { v = recover() }()
			_ = format.ServeBlob(rr, req, c.p, blobName, d, format.ServeBlobOptions{ContentType: "application/octet-stream", Decrypt: c.key})
		}()
		if err, _ := v.(error); !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("%s: ServeBlob ended with %v, want an abort", name, v)
		}
		if rr.Body.Len() >= len(plain) {
			t.Errorf("%s: %d bytes of %d went out", name, rr.Body.Len(), len(plain))
		}
	}
}

// closerOf is r with a Close that does nothing.
func closerOf(r *bytes.Reader) interface {
	Read([]byte) (int, error)
	Close() error
} {
	return struct {
		*bytes.Reader
		closerFunc
	}{r, closerFunc(func() error { return nil })}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
