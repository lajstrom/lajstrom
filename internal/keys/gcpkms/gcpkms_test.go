package gcpkms

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
)

const testVersion = "projects/p/locations/europe-west1/keyRings/lajstrom/cryptoKeys/apt/cryptoKeyVersions/1"

// fakeKMS is Cloud KMS's REST API for one key version, testVersion, that wants the bearer token
// "token-1".
type fakeKMS struct {
	t    *testing.T
	priv crypto.Signer

	mu        sync.Mutex
	version   map[string]any
	fail      []int  // the statuses of the next answers, UNAVAILABLE for a 5xx
	refuse    string // a 403 PERMISSION_DENIED with this message while set
	badCRC    bool   // answers a signature with a CRC32C of other bytes
	requests  []string
	createdAt time.Time
}

func newFakeKMS(t *testing.T, priv crypto.Signer, algorithm string) (*fakeKMS, *Client) {
	t.Helper()
	f := &fakeKMS{t: t, priv: priv, createdAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	f.version = map[string]any{"name": testVersion, "state": "ENABLED", "algorithm": algorithm,
		"createTime": "2026-03-01T12:00:00.123456789Z", "protectionLevel": "HSM"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New(Options{Endpoint: srv.URL, Log: slog.New(slog.DiscardHandler),
		Token: func(context.Context) (string, error) { return "token-1", nil }})
	c.backoff = time.Millisecond
	return f, c
}

func (f *fakeKMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer token-1" {
		apiError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "no token")
		return
	}
	if len(f.fail) > 0 {
		status := f.fail[0]
		f.fail = f.fail[1:]
		apiError(w, status, map[bool]string{true: "UNAVAILABLE", false: "RESOURCE_EXHAUSTED"}[status >= 500], "try again")
		return
	}
	if f.refuse != "" {
		apiError(w, http.StatusForbidden, "PERMISSION_DENIED", f.refuse)
		return
	}
	switch path := strings.TrimPrefix(r.URL.Path, "/v1/"); {
	case r.Method == http.MethodGet && path == testVersion:
		_ = json.NewEncoder(w).Encode(f.version)
	case r.Method == http.MethodGet && path == testVersion+"/publicKey":
		der, _ := x509.MarshalPKIXPublicKey(f.priv.Public())
		p := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
		_ = json.NewEncoder(w).Encode(map[string]any{"pem": string(p), "algorithm": f.version["algorithm"], "pemCrc32c": crc(p)})
	case r.Method == http.MethodPost && path == testVersion+":asymmetricSign":
		f.sign(w, r)
	default:
		apiError(w, http.StatusNotFound, "NOT_FOUND", "no "+path)
	}
}

// sign answers asymmetricSign: the signature of the digest, checked against its CRC32C.
func (f *fakeKMS) sign(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Digest       struct{ Sha256 []byte }
		DigestCrc32c string
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Digest.Sha256) != 32 || in.DigestCrc32c != crc(in.Digest.Sha256) {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "digest")
		return
	}
	sig, err := f.priv.Sign(rand.Reader, in.Digest.Sha256, crypto.SHA256)
	if err != nil {
		f.t.Error(err)
	}
	sum := crc(sig)
	if f.badCRC {
		sum = crc(append(sig, 0))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"name": testVersion, "signature": sig, "signatureCrc32c": sum,
		"verifiedDigestCrc32c": true, "protectionLevel": "HSM"})
}

func crc(b []byte) string {
	return strconv.FormatUint(uint64(crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))), 10)
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": status, "status": code, "message": msg}})
}

func (f *fakeKMS) set(fn func(*fakeKMS)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// TestKeyAndSign describes a key version, RSA and P-256, with its creation time and public
// key, and signs a digest with it through keys.Service: the repo's key is the version's, and its
// InRelease and Release.gpg verify against key.asc.
func TestKeyAndSign(t *testing.T) {
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	e, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	for algorithm, priv := range map[string]crypto.Signer{"RSA_SIGN_PKCS1_2048_SHA256": r, "EC_SIGN_P256_SHA256": e} {
		t.Run(algorithm, func(t *testing.T) {
			ctx := t.Context()
			f, c := newFakeKMS(t, priv, algorithm)
			k, err := c.Key(ctx, testVersion)
			must(t, err)
			if k.Name != testVersion || !k.Created.Equal(time.Date(2026, 3, 1, 12, 0, 0, 123456789, time.UTC)) {
				t.Errorf("Key = %s created %v", k.Name, k.Created)
			}

			svc, repo := newService(t, c, "gcpkms://"+testVersion)
			pub, err := svc.PublicKeys(ctx, repo)
			must(t, err)
			kr, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(pub))
			must(t, err)
			if len(kr) != 1 || !kr[0].PrimaryKey.CreationTime.Equal(f.createdAt) {
				t.Errorf("key.asc: %d keys, created %v, want %v", len(kr), kr[0].PrimaryKey.CreationTime, f.createdAt)
			}
			release := []byte("Origin: Lajstrom\n")
			sig, err := svc.DetachSign(ctx, repo, release)
			must(t, err)
			if _, err := openpgp.CheckArmoredDetachedSignature(kr, bytes.NewReader(release), bytes.NewReader(sig), nil); err != nil {
				t.Errorf("Release.gpg: %v", err)
			}
			in, err := svc.ClearSign(ctx, repo, release)
			must(t, err)
			if !bytes.Contains(in, []byte("BEGIN PGP SIGNATURE")) {
				t.Errorf("InRelease:\n%s", in)
			}
		})
	}
}

// newService is a keys.Service over SQLite in memory with c as its GCP KMS and keys.kms uri, and
// the repo apt/ubuntu.
func newService(t *testing.T, c *Client, uri string) (*keys.Service, index.Repo) {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:gcpkms-"+strings.ReplaceAll(t.Name(), "/", "-")+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repo := index.Repo{Format: "apt", Name: "ubuntu", Registry: "local", Prefix: "apt/ubuntu", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &repo))
	m, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
	must(t, err)
	svc, err := keys.New(keys.Options{Index: st, Master: m, Log: slog.New(slog.DiscardHandler),
		KMS: map[string]keys.KMS{config.KMSSchemeGCP: c}, KMSKey: uri})
	must(t, err)
	return svc, repo
}

// TestKeyUnsupported: a version that is not enabled, or of an algorithm that is not PKCS #1 v1.5
// or ECDSA with SHA-256, is keys.ErrKMSKeyUnsupported naming what it is.
func TestKeyUnsupported(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	for _, tt := range []struct{ field, value, want string }{
		{"state", "DISABLED", "is DISABLED"},
		{"state", "DESTROY_SCHEDULED", "is DESTROY_SCHEDULED"},
		{"algorithm", "EC_SIGN_P384_SHA384", "a version of EC_SIGN_P384_SHA384"},
		{"algorithm", "EC_SIGN_ED25519", "a version of EC_SIGN_ED25519"},
		{"algorithm", "RSA_SIGN_PSS_4096_SHA256", "a version of RSA_SIGN_PSS_4096_SHA256"},
		{"algorithm", "RSA_SIGN_RAW_PKCS1_4096", "a version of RSA_SIGN_RAW_PKCS1_4096"},
	} {
		t.Run(tt.value, func(t *testing.T) {
			f, c := newFakeKMS(t, priv, "EC_SIGN_P256_SHA256")
			f.set(func(f *fakeKMS) { f.version[tt.field] = tt.value })
			_, err := c.Key(t.Context(), testVersion)
			if !errors.Is(err, keys.ErrKMSKeyUnsupported) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Key = %v, want ErrKMSKeyUnsupported with %q", err, tt.want)
			}
		})
	}
}

// TestUnavailable: a 5xx or a 429 is sent again with backoff, Retries times, and is then
// keys.ErrKMSUnavailable, as are a KMS that does not answer, a token that cannot be had and a
// signature whose CRC32C is wrong; a refusal such as PERMISSION_DENIED is sent once and is not.
func TestUnavailable(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	digest := make([]byte, 32)
	ctx := t.Context()
	f, c := newFakeKMS(t, priv, "EC_SIGN_P256_SHA256")

	f.set(func(f *fakeKMS) { f.fail = []int{503, 429} })
	if _, err := c.Sign(ctx, testVersion, priv.Public(), digest); err != nil || len(f.requests) != 3 {
		t.Errorf("after two unavailable answers: %v, %d requests", err, len(f.requests))
	}
	f.set(func(f *fakeKMS) { f.requests, f.fail = nil, []int{500, 503, 502} })
	_, err = c.Sign(ctx, testVersion, priv.Public(), digest)
	if !errors.Is(err, keys.ErrKMSUnavailable) || !strings.Contains(err.Error(), "UNAVAILABLE") || len(f.requests) != 3 {
		t.Errorf("three unavailable answers: %v, %d requests", err, len(f.requests))
	}
	f.set(func(f *fakeKMS) {
		f.requests, f.refuse = nil, "Permission 'cloudkms.cryptoKeyVersions.useToSign' denied"
	})
	_, err = c.Sign(ctx, testVersion, priv.Public(), digest)
	if err == nil || errors.Is(err, keys.ErrKMSUnavailable) || !strings.Contains(err.Error(), "403 PERMISSION_DENIED") || len(f.requests) != 1 {
		t.Errorf("a refusal: %v, %d requests", err, len(f.requests))
	}
	f.set(func(f *fakeKMS) { f.refuse, f.badCRC = "", true })
	if _, err := c.Sign(ctx, testVersion, priv.Public(), digest); !errors.Is(err, keys.ErrKMSUnavailable) || !strings.Contains(err.Error(), "CRC32C") {
		t.Errorf("a signature whose CRC32C is wrong: %v", err)
	}

	noToken := New(Options{Endpoint: c.o.Endpoint, Retries: -1, Log: c.log,
		Token: func(context.Context) (string, error) { return "", errors.New("gcp metadata server: 503") }})
	if _, err := noToken.Sign(ctx, testVersion, priv.Public(), digest); !errors.Is(err, keys.ErrKMSUnavailable) {
		t.Errorf("no token: %v", err)
	}
	down := New(Options{Endpoint: "http://127.0.0.1:1", Retries: -1, Log: c.log, Token: c.o.Token})
	_, err = down.Sign(ctx, testVersion, priv.Public(), digest)
	if !errors.Is(err, keys.ErrKMSUnavailable) || strings.Contains(err.Error(), "cryptoKeyVersions") {
		t.Errorf("no KMS: %v, want ErrKMSUnavailable without the URL", err)
	}
}

// TestMetadataToken: without Options.Token the client asks the metadata server of
// MetadataHost for the access token, with Metadata-Flavor: Google, and sends it to the KMS.
func TestMetadataToken(t *testing.T) {
	var asked int
	var mu sync.Mutex
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		mu.Unlock()
		if r.Header.Get("Metadata-Flavor") != "Google" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Metadata-Flavor", "Google")
		_, _ = io.WriteString(w, `{"access_token":"token-1","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(md.Close)
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	f, fc := newFakeKMS(t, priv, "EC_SIGN_P256_SHA256")
	c := New(Options{Endpoint: fc.o.Endpoint, MetadataHost: strings.TrimPrefix(md.URL, "http://"), Log: slog.New(slog.DiscardHandler)})
	for range 2 {
		if _, err := c.Key(t.Context(), testVersion); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if asked != 1 || len(f.requests) != 4 {
		t.Errorf("%d requests to the metadata server, %d to the KMS; want 1 and 4", asked, len(f.requests))
	}
	bad := New(Options{Endpoint: fc.o.Endpoint, MetadataHost: "http://metadata/", Log: slog.New(slog.DiscardHandler)})
	if _, err := bad.Key(t.Context(), testVersion); err == nil || !strings.Contains(err.Error(), "GCE_METADATA_HOST") {
		t.Errorf("a metadata host that is a URL: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(fmt.Sprint(err))
	}
}
