package awskms

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/keys/keystest"
)

// testAD is the additional data of the master key 0123456789abcdef (#1388).
var testAD = keys.AdditionalData{Key: "lajstrom", Value: "master key 0123456789abcdef"}

// wrapServer is keystest's AWS KMS of symmetric keys behind a check of every request: SigV4, the
// operation, and no EncryptionAlgorithm but the default; fail answers the next requests with
// these statuses first.
type wrapServer struct {
	t    *testing.T
	kms  *keystest.WrapKMS
	next http.Handler

	mu   sync.Mutex
	fail []int
	ins  []map[string]any
}

func newWrapServer(t *testing.T) (*wrapServer, *Client) {
	t.Helper()
	k := keystest.NewWrapKMS()
	s := &wrapServer{t: t, kms: k, next: keystest.AWSWrapKMS(k)}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c := New(Options{Endpoint: srv.URL, Region: "eu-west-1", Log: slog.New(slog.DiscardHandler),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIDTEST", SecretAccessKey: "secret"}, nil
		})})
	c.backoff = time.Millisecond
	return s, c
}

func (s *wrapServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	auth := r.Header.Get("Authorization")
	if !strings.Contains(auth, "/eu-west-1/kms/aws4_request") {
		s.t.Errorf("%s: not SigV4 of kms in the region of the key's ARN: %q", r.Header.Get("X-Amz-Target"), auth)
	}
	if a, ok := in["EncryptionAlgorithm"]; ok && a != "SYMMETRIC_DEFAULT" {
		s.t.Errorf("%s with the algorithm %v", r.Header.Get("X-Amz-Target"), a)
	}
	s.mu.Lock()
	s.ins = append(s.ins, in)
	var status int
	if len(s.fail) > 0 {
		status, s.fail = s.fail[0], s.fail[1:]
	}
	s.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"__type":"ThrottlingException","message":"slow down"}`)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.next.ServeHTTP(w, r)
}

// TestSymmetricKey: DescribeKey resolves an alias to the key's ARN, and a key that is not an
// enabled symmetric key for encryption is ErrKMSKeyCannotWrap, naming its spec, usage or state.
func TestSymmetricKey(t *testing.T) {
	s, c := newWrapServer(t)
	for _, name := range []string{s.kms.ARN, s.kms.Alias} {
		if arn, err := c.SymmetricKey(t.Context(), name); err != nil || arn != s.kms.ARN {
			t.Errorf("SymmetricKey(%s) = %q, %v; want the ARN", name, arn, err)
		}
	}
	for desc, want := range map[string]string{
		"RSA_2048 ENCRYPT_DECRYPT Enabled":           "a key of RSA_2048 for ENCRYPT_DECRYPT, not SYMMETRIC_DEFAULT for ENCRYPT_DECRYPT",
		"HMAC_256 GENERATE_VERIFY_MAC Enabled":       "a key of HMAC_256 for GENERATE_VERIFY_MAC",
		"SYMMETRIC_DEFAULT ENCRYPT_DECRYPT Disabled": "is Disabled, not Enabled",
	} {
		s.kms.Disable(s.kms.ARN, desc)
		_, err := c.SymmetricKey(t.Context(), s.kms.Alias)
		if !errors.Is(err, keys.ErrKMSKeyCannotWrap) || !strings.Contains(err.Error(), want) {
			t.Errorf("SymmetricKey of %s: %v, want ErrKMSKeyCannotWrap naming %q", desc, err, want)
		}
	}
}

// wrapOnce wraps 32 bytes under testAD with c and the first key of s, and returns them and the
// ciphertext.
func wrapOnce(t *testing.T, s *wrapServer, c *Client) (plain, wrapped []byte) {
	t.Helper()
	plain = bytes.Repeat([]byte{0x5a}, 32)
	wrapped, err := c.Encrypt(t.Context(), s.kms.ARN, plain, testAD)
	must(t, err)
	return plain, wrapped
}

// noSecret fails the test when err holds one of the byte strings of secrets, as they are or in
// base64.
func noSecret(t *testing.T, what string, err error, secrets ...[]byte) {
	t.Helper()
	for _, s := range secrets {
		if err != nil && (strings.Contains(err.Error(), string(s)) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(s))) {
			t.Errorf("%s: the error %q holds the key or the ciphertext", what, err)
		}
	}
}

// TestEncryptDecryptSymmetric: what Encrypt wraps under an encryption context, DecryptSymmetric
// unwraps under it, and under another context the KMS refuses it; each request carries the
// context {"lajstrom": "master key <id>"}; no error holds the key or the ciphertext.
func TestEncryptDecryptSymmetric(t *testing.T) {
	s, c := newWrapServer(t)
	ctx := t.Context()
	plain, wrapped := wrapOnce(t, s, c)
	got, err := c.DecryptSymmetric(ctx, s.kms.ARN, wrapped, testAD)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("DecryptSymmetric = %x, %v; want the plaintext", got, err)
	}
	for _, in := range s.ins {
		if ec, _ := json.Marshal(in["EncryptionContext"]); string(ec) != `{"lajstrom":"master key 0123456789abcdef"}` {
			t.Errorf("the encryption context %s, want {\"lajstrom\": \"master key 0123456789abcdef\"}", ec)
		}
	}
	other := keys.AdditionalData{Key: "lajstrom", Value: "master key fedcba9876543210"}
	_, err = c.DecryptSymmetric(ctx, s.kms.ARN, wrapped, other)
	if err == nil || errors.Is(err, keys.ErrKMSUnavailable) || !strings.Contains(err.Error(), "InvalidCiphertextException") {
		t.Errorf("DecryptSymmetric under another context: %v, want the KMS's InvalidCiphertextException", err)
	}
	noSecret(t, "another context", err, plain, wrapped)
}

// TestSymmetricUnavailable: a KMS that throttles is asked again, then unavailable; a refusal is
// not unavailable; no error holds the key or the ciphertext.
func TestSymmetricUnavailable(t *testing.T) {
	s, c := newWrapServer(t)
	ctx := t.Context()
	plain, wrapped := wrapOnce(t, s, c)
	s.mu.Lock()
	s.fail = []int{http.StatusBadRequest, http.StatusServiceUnavailable}
	s.mu.Unlock()
	if got, err := c.DecryptSymmetric(ctx, s.kms.ARN, wrapped, testAD); err != nil || !bytes.Equal(got, plain) {
		t.Errorf("DecryptSymmetric after two throttled answers = %v, want it asked again", err)
	}
	s.mu.Lock()
	s.fail = []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable}
	s.mu.Unlock()
	_, err := c.DecryptSymmetric(ctx, s.kms.ARN, wrapped, testAD)
	if !errors.Is(err, keys.ErrKMSUnavailable) {
		t.Errorf("DecryptSymmetric of a KMS that does not answer: %v, want ErrKMSUnavailable", err)
	}
	noSecret(t, "unavailable", err, plain, wrapped)
	s.kms.Fail(errors.New("AccessDeniedException: not this role"))
	for _, err := range []error{second(c.Encrypt(ctx, s.kms.ARN, plain, testAD)), second(c.DecryptSymmetric(ctx, s.kms.ARN, wrapped, testAD))} {
		if err == nil || errors.Is(err, keys.ErrKMSUnavailable) || !strings.Contains(err.Error(), "AccessDeniedException") {
			t.Errorf("a refusal: %v, want AccessDeniedException, not unavailable", err)
		}
		noSecret(t, "a refusal", err, plain, wrapped)
	}
}

// TestDecryptSymmetricOtherKey: an answer that names another key than the one asked is an error,
// and no plaintext.
func TestDecryptSymmetricOtherKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"KeyId": "arn:aws:kms:eu-west-1:111122223333:key/other",
			"Plaintext": bytes.Repeat([]byte{1}, 32), "CiphertextBlob": []byte("c")})
	}))
	t.Cleanup(srv.Close)
	c := New(Options{Endpoint: srv.URL, Region: "eu-west-1", Log: slog.New(slog.DiscardHandler),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIDTEST", SecretAccessKey: "secret"}, nil
		})})
	const arn = "arn:aws:kms:eu-west-1:111122223333:key/keystest-wrap"
	if p, err := c.DecryptSymmetric(t.Context(), arn, []byte("c"), testAD); p != nil || err == nil ||
		!strings.Contains(err.Error(), "answered with the key") {
		t.Errorf("DecryptSymmetric answered by another key = %x, %v; want an error and no plaintext", p, err)
	}
	if c, err := c.Encrypt(t.Context(), arn, []byte("p"), testAD); c != nil || err == nil {
		t.Errorf("Encrypt answered by another key = %x, %v; want an error", c, err)
	}
}

// second returns the error of a call of two results.
func second[T any](_ T, err error) error { return err }
