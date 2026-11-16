package facade

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"net/http"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestVerdictPushNeedsAttach (ADR 0008, amendment I; rbac.md §2): a verdict pushed through /v2 is
// a referrer, which needs attach, and write holds it: a principal with read and attach pushes
// one, 201, a writer too, a reader gets 403.
func TestVerdictPushNeedsAttach(t *testing.T) {
	e := newEnv(t, envOptions{authz: authorizer{
		"alice": {"raw/assets": {auth.ActionRead, auth.ActionWrite}},
		"scan":  {"raw/assets": {auth.ActionRead, auth.ActionAttach}},
		"bob":   {"raw/assets": {auth.ActionRead}},
	}})
	f, subject := e.signedFile("docs/judged.txt")
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	must(t, err)
	sign := func(_ context.Context, msg []byte) ([]byte, error) {
		sum := sha256.Sum256(msg)
		return ecdsa.SignASN1(rand.Reader, priv, sum[:])
	}
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: subject}
	for _, tt := range []struct {
		who    string
		reason string
		status int
	}{
		{"bob", "a reader's", http.StatusForbidden},
		{"scan", "an attacher's", http.StatusCreated},
		{"alice", "a writer's", http.StatusCreated},
	} {
		v, err := attest.BuildVerdict(t.Context(), "raw/assets/"+f.path, desc,
			attest.Verdict{Verdict: "deny", Reason: tt.reason + " verdict", Source: "review"}, der, sign, e.clock.Now())
		must(t, err)
		e.pushBlob("raw/assets/"+f.path, v.Bundle)
		rr := e.do(e.h, http.MethodPut, "/v2/raw/assets/"+f.path+"/manifests/"+v.Desc.Digest.String(), bytes.NewReader(v.Manifest),
			"Content-Type", ocispecManifest, principalHeader, tt.who)
		wantStatus(t, tt.reason+" verdict", rr, tt.status)
	}
}
