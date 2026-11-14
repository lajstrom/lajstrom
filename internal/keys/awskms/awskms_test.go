package awskms

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
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
)

const (
	testARN   = "arn:aws:kms:eu-central-1:111122223333:key/0e9f6c2a-test"
	testAlias = "alias/lajstrom-apt"
)

// fakeKMS is an AWS KMS of the JSON 1.1 API over HTTP that holds one key under testARN and
// testAlias, and checks that each request is SigV4 of kms in eu-central-1.
type fakeKMS struct {
	t    *testing.T
	priv crypto.Signer
	meta map[string]any // the KeyMetadata of DescribeKey, which a test may change

	mu      sync.Mutex
	fail    []int // the statuses of the next answers, ThrottlingException for a 400
	refuse  string
	ops     []string
	created time.Time
}

func newFakeKMS(t *testing.T, priv crypto.Signer, spec string) (*fakeKMS, *Client) {
	t.Helper()
	algos := []string{"RSASSA_PSS_SHA_256", algorithmRSA}
	if spec == "ECC_NIST_P256" {
		algos = []string{algorithmECDSA}
	}
	f := &fakeKMS{t: t, priv: priv, created: time.Unix(1794652453, 0),
		meta: map[string]any{"Arn": testARN, "CreationDate": 1794652453.123, "KeyUsage": "SIGN_VERIFY", "KeySpec": spec,
			"KeyState": "Enabled", "SigningAlgorithms": algos}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New(Options{Endpoint: srv.URL, Region: "eu-central-1", Log: slog.New(slog.DiscardHandler),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIDTEST", SecretAccessKey: "secret"}, nil
		})})
	c.backoff = time.Millisecond
	return f, c
}

func (f *fakeKMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "TrentService.")
	f.ops = append(f.ops, op)
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDTEST/") || !strings.Contains(auth, "/eu-central-1/kms/aws4_request") ||
		r.Header.Get("X-Amz-Date") == "" || r.Header.Get("Content-Type") != "application/x-amz-json-1.1" {
		f.t.Errorf("%s: not SigV4 of kms in eu-central-1: %q", op, auth)
	}
	if f.failed(w) {
		return
	}
	var in struct {
		KeyID            string `json:"KeyId"`
		Message          []byte
		MessageType      string
		SigningAlgorithm string
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		f.t.Errorf("%s: %v", op, err)
	}
	var out any
	switch op {
	case "DescribeKey":
		if in.KeyID != testARN && in.KeyID != testAlias {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"__type":"NotFoundException","message":"not found"}`)
			return
		}
		out = map[string]any{"KeyMetadata": f.meta}
	case "GetPublicKey":
		der, _ := x509.MarshalPKIXPublicKey(f.priv.Public())
		out = map[string]any{"KeyId": testARN, "PublicKey": der}
	case "Sign":
		if in.KeyID != testARN || in.MessageType != "DIGEST" || len(in.Message) != sha256.Size {
			f.t.Errorf("Sign of %s, %s, %d bytes", in.KeyID, in.MessageType, len(in.Message))
		}
		out = map[string]any{"KeyId": testARN, "Signature": f.sign(in.Message, in.SigningAlgorithm), "SigningAlgorithm": in.SigningAlgorithm}
	}
	_ = json.NewEncoder(w).Encode(out)
}

// sign signs digest with the key, in algorithm.
func (f *fakeKMS) sign(digest []byte, algorithm string) []byte {
	var opts crypto.SignerOpts = crypto.SHA256
	if algorithm == "RSASSA_PSS_SHA_256" {
		opts = &rsa.PSSOptions{Hash: crypto.SHA256}
	}
	sig, err := f.priv.Sign(rand.Reader, digest, opts)
	if err != nil {
		f.t.Error(err)
	}
	return sig
}

// failed answers the next failure the test set, and reports whether there was one.
func (f *fakeKMS) failed(w http.ResponseWriter) bool {
	switch {
	case len(f.fail) > 0:
		status := f.fail[0]
		f.fail = f.fail[1:]
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"__type":"com.amazonaws.kms#ThrottlingException","message":"Rate exceeded"}`)
	case f.refuse != "":
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"__type":"`+f.refuse+`","message":"User: arn:aws:iam::111122223333:role/lajstrom is not authorized"}`)
	default:
		return false
	}
	return true
}

func (f *fakeKMS) set(fn func(*fakeKMS)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// TestKeyAndSign describes a key by its alias, which resolves to its ARN, with its public key and
// its creation date to the second, and signs a digest with it by the ARN: PKCS #1 v1.5 for RSA,
// ECDSA for P-256, verified with the public key.
func TestKeyAndSign(t *testing.T) {
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	e, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	for spec, priv := range map[string]crypto.Signer{"RSA_4096": r, "ECC_NIST_P256": e} {
		t.Run(spec, func(t *testing.T) {
			f, c := newFakeKMS(t, priv, spec)
			k, err := c.Key(t.Context(), testAlias)
			must(t, err)
			if k.Name != testARN || !k.Created.Equal(f.created) {
				t.Errorf("Key = %s created %v, want %s created %v", k.Name, k.Created, testARN, f.created)
			}
			digest := sha256.Sum256([]byte("Release"))
			sig, err := c.Sign(t.Context(), k.Name, k.Public, digest[:])
			must(t, err)
			switch pub := k.Public.(type) {
			case *rsa.PublicKey:
				must(t, rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig))
			case *ecdsa.PublicKey:
				if !ecdsa.VerifyASN1(pub, digest[:], sig) {
					t.Error("the ECDSA signature does not verify")
				}
			}
			if strings.Join(f.ops, ",") != "DescribeKey,GetPublicKey,Sign" {
				t.Errorf("requests %v", f.ops)
			}
		})
	}
}

// TestKeyUnsupported: a key whose usage, spec, state or algorithms are not what OpenPGP signing
// needs is keys.ErrKMSKeyUnsupported naming what it is.
func TestKeyUnsupported(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	for _, tt := range []struct{ field, value, want string }{
		{"KeyUsage", "ENCRYPT_DECRYPT", "usage ENCRYPT_DECRYPT"},
		{"KeySpec", "ECC_NIST_P384", "a key of ECC_NIST_P384"},
		{"KeySpec", "ECC_NIST_EDWARDS25519", "a key of ECC_NIST_EDWARDS25519"},
		{"KeyState", "PendingDeletion", "is PendingDeletion"},
		{"SigningAlgorithms", "", "does not sign with ECDSA_SHA_256"},
	} {
		t.Run(tt.value, func(t *testing.T) {
			f, c := newFakeKMS(t, priv, "ECC_NIST_P256")
			f.set(func(f *fakeKMS) {
				f.meta[tt.field] = tt.value
				if tt.field == "SigningAlgorithms" {
					f.meta[tt.field] = []string{}
				}
			})
			_, err := c.Key(t.Context(), testARN)
			if !errors.Is(err, keys.ErrKMSKeyUnsupported) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Key = %v, want ErrKMSKeyUnsupported with %q", err, tt.want)
			}
		})
	}
}

// TestUnavailable: a 5xx, a 429 or a ThrottlingException is sent again with backoff, Retries
// times, and then is keys.ErrKMSUnavailable, as is a KMS that does not answer; a refusal such as
// AccessDeniedException is sent once and is not.
func TestUnavailable(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	digest := make([]byte, sha256.Size)

	f, c := newFakeKMS(t, priv, "ECC_NIST_P256")
	f.set(func(f *fakeKMS) { f.fail = []int{http.StatusServiceUnavailable, http.StatusBadRequest} })
	if _, err := c.Sign(t.Context(), testARN, priv.Public(), digest); err != nil || len(f.ops) != 3 {
		t.Errorf("after two unavailable answers: %v, %d requests", err, len(f.ops))
	}
	f.set(func(f *fakeKMS) { f.ops, f.fail = nil, []int{500, 429, 400} })
	_, err = c.Sign(t.Context(), testARN, priv.Public(), digest)
	if !errors.Is(err, keys.ErrKMSUnavailable) || !strings.Contains(err.Error(), "ThrottlingException") || len(f.ops) != 3 {
		t.Errorf("three unavailable answers: %v, %d requests", err, len(f.ops))
	}
	f.set(func(f *fakeKMS) { f.ops, f.refuse = nil, "AccessDeniedException" })
	_, err = c.Sign(t.Context(), testARN, priv.Public(), digest)
	if err == nil || errors.Is(err, keys.ErrKMSUnavailable) || !strings.Contains(err.Error(), "400 AccessDeniedException") || len(f.ops) != 1 {
		t.Errorf("a refusal: %v, %d requests", err, len(f.ops))
	}

	down := New(Options{Endpoint: "http://127.0.0.1:1", Retries: -1, Credentials: c.o.Credentials, Log: c.log})
	if _, err := down.Sign(t.Context(), testARN, priv.Public(), digest); !errors.Is(err, keys.ErrKMSUnavailable) {
		t.Errorf("no KMS: %v", err)
	}
}

// TestEndpoint: the endpoint of a key is the configured one, else the KMS endpoint of the
// region of its ARN in the domain of its partition, or of the default region.
func TestEndpoint(t *testing.T) {
	for _, tt := range []struct{ base, name, region, want, err string }{
		{name: testARN, want: "https://kms.eu-central-1.amazonaws.com/"},
		{name: "arn:aws-cn:kms:cn-north-1:111122223333:alias/apt", want: "https://kms.cn-north-1.amazonaws.com.cn/"},
		{name: "arn:aws-us-gov:kms:us-gov-west-1:111122223333:key/k", want: "https://kms.us-gov-west-1.amazonaws.com/"},
		{name: "alias/apt", region: "cn-northwest-1", want: "https://kms.cn-northwest-1.amazonaws.com.cn/"},
		{name: "alias/apt", region: "eu-west-1", want: "https://kms.eu-west-1.amazonaws.com/"},
		{base: "https://vpce-1.kms.eu-west-1.vpce.amazonaws.com", name: testARN, want: "https://vpce-1.kms.eu-west-1.vpce.amazonaws.com/"},
		{name: "arn:aws-iso:kms:us-iso-east-1:111122223333:key/k", err: "set AWS_ENDPOINT_URL_KMS"},
		{base: "kms.local", name: testARN, err: "not an http(s) URL"},
	} {
		partition, region := "", tt.region
		if a, ok := parseARN(tt.name); ok {
			partition, region = a.partition, a.region
		}
		got, err := endpointOf(tt.base, partition, region)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s: %q, %v; want an error with %q", tt.name, got, err, tt.err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("%s in %q: %q, %v; want %q", tt.name, tt.region, got, err, tt.want)
		}
	}
	c := New(Options{Credentials: aws.AnonymousCredentials{}})
	if _, err := c.Key(t.Context(), "alias/apt"); err == nil || !strings.Contains(err.Error(), "names no region") {
		t.Errorf("a key without a region: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
