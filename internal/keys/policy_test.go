package keys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
)

// verifyPolicy checks sig, of msg, with the public key of pemKey, as cosign verify-blob does.
func verifyPolicy(t *testing.T, pemKey, msg, sig []byte) bool {
	t.Helper()
	block, _ := pem.Decode(pemKey)
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatalf("not a PEM public key: %q", pemKey)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	must(t, err)
	sum := sha256.Sum256(msg)
	return ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), sum[:], sig)
}

// TestPolicyKeyLocal: the first call makes the key, sealed with the master key; the next calls
// read the same key; it signs what its PEM verifies; another master key does not open it.
func TestPolicyKeyLocal(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := t.Context()
	k, err := e.svc.PolicyKey(ctx, "")
	must(t, err)
	again, err := e.svc.PolicyKey(ctx, Local)
	must(t, err)
	if k.Fingerprint() != again.Fingerprint() || k.Key.Kind != KindPolicy || k.Key.RepoID != nil {
		t.Fatalf("the key again: %s, %s, kind %s", k, again, k.Key.Kind)
	}
	sig, err := again.Sign(ctx, []byte("the deny list"))
	must(t, err)
	if !verifyPolicy(t, k.PEM(), []byte("the deny list"), sig) || verifyPolicy(t, k.PEM(), []byte("another"), sig) {
		t.Error("the signature does not verify with the PEM, or verifies another message")
	}
	other, err := New(Options{Index: e.st, Master: master(t, "11"+testMasterHex[2:])})
	must(t, err)
	if _, err := other.PolicyKey(ctx, ""); err == nil {
		t.Error("another master key opened the policy key")
	}
}

// TestPolicyKeyKMS: a policy key of a KMS key URI signs through the KMS, and a key that is not
// ECDSA on P-256 is refused.
func TestPolicyKeyKMS(t *testing.T) {
	f := newFakeKMS()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.add("arn:aws:kms:eu-west-1:1:key/p", priv, created, "alias/policy")
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	f.add("arn:aws:kms:eu-west-1:1:key/r", rsaKey, created)
	e := newEnv(t, Options{KMS: map[string]KMS{config.KMSSchemeAWS: f}})
	ctx := t.Context()
	if _, err := e.svc.PolicyKey(ctx, "awskms:///arn:aws:kms:eu-west-1:1:key/r"); !errors.Is(err, ErrKMSKeyUnsupported) {
		t.Errorf("an RSA key: %v", err)
	}
	k, err := e.svc.PolicyKey(ctx, "awskms:///alias/policy")
	must(t, err)
	sig, err := k.Sign(ctx, []byte("a verdict"))
	must(t, err)
	if !verifyPolicy(t, k.PEM(), []byte("a verdict"), sig) || f.calls() != 1 {
		t.Errorf("the KMS signature: verified %v, %d calls", verifyPolicy(t, k.PEM(), []byte("a verdict"), sig), f.calls())
	}
	again, err := e.svc.PolicyKey(ctx, "")
	must(t, err)
	if again.Fingerprint() != k.Fingerprint() || again.kmsName != "arn:aws:kms:eu-west-1:1:key/p" {
		t.Errorf("the key read again: %s in %q, want the KMS key by its canonical name", again, again.kmsName)
	}
}
