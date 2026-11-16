package raw

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// encryptedEnv is newEnv of a repo with policy.encryption at-rest and blobRedirect auto, on a
// registry that redirects every blob.
func encryptedEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, envOptions{policy: `{"encryption":{"mode":"at-rest"},"blobRedirect":"auto"}`,
		enc: func(st index.Store) format.Encryption {
			m, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
			must(t, err)
			enc, err := keys.NewEncryption(keys.EncryptionOptions{Index: st, Master: m, Log: slog.New(slog.DiscardHandler)})
			must(t, err)
			return enc
		}})
	e.prov.RedirectURL = func(_ string, d digest.Digest) (string, bool) { return "https://objects.test/" + d.Encoded(), true }
	return e
}

// TestEncryptedRaw: a file PUT to an encrypted repo is ciphertext in the registry, and GET serves
// it decrypted, whole and in ranges, never with a 307 though the registry and the policy would
// redirect.
func TestEncryptedRaw(t *testing.T) {
	e := encryptedEnv(t)
	const plain = "the bytes of a private file, never in the registry in the clear"
	resp := e.put("dist/secret.txt", plain)
	v := e.versions("dist/secret.txt")
	if len(v) != 1 {
		t.Fatalf("versions %+v", v)
	}
	files, err := e.st.ListFiles(t.Context(), v[0].ID)
	must(t, err)
	if len(files) != 1 || files[0].Digest == digest.FromString(plain) {
		t.Fatalf("files %+v: want the ciphertext's digest", files)
	}
	rr, err := e.prov.Repository(t.Context(), "raw/scratch/dist/secret.txt")
	must(t, err)
	rc, err := rr.Fetch(t.Context(), ocispec.Descriptor{Digest: files[0].Digest, Size: files[0].Size})
	must(t, err)
	stored, err := io.ReadAll(rc)
	_ = rc.Close()
	must(t, err)
	if bytes.Contains(stored, []byte("private file")) {
		t.Fatal("the registry holds the plaintext")
	}
	if resp.Version != digest.FromString(plain).Encoded()[:16] {
		t.Errorf("the version %q is not the plaintext's sha256", resp.Version)
	}

	got := e.do(http.MethodGet, target("dist/secret.txt"), nil)
	if got.Code != http.StatusOK || got.Body.String() != plain || got.Header().Get("Location") != "" {
		t.Fatalf("GET = %d %q %v; want the plaintext, no redirect", got.Code, got.Body, got.Header())
	}
	part := e.do(http.MethodGet, target("dist/secret.txt"), nil, "Range", "bytes=4-8")
	if part.Code != http.StatusPartialContent || part.Body.String() != plain[4:9] {
		t.Errorf("GET of bytes 4-8 = %d %q, want %q", part.Code, part.Body, plain[4:9])
	}
	head := e.do(http.MethodHead, target("dist/secret.txt"), nil)
	if head.Code != http.StatusOK || head.Header().Get("Content-Length") != strconv.Itoa(len(plain)) {
		t.Errorf("HEAD = %d, Content-Length %s", head.Code, head.Header().Get("Content-Length"))
	}
}
