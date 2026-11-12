package rpm

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// newSignedEnv returns an env whose routes sign with the keys service of internal/keys, with a
// master key of the test, which it also returns.
func newSignedEnv(t *testing.T) (*env, *keys.Service) {
	t.Helper()
	var svc *keys.Service
	e := newEnv(t, envOptions{signer: func(st index.Store) format.Signer {
		m, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
		must(t, err)
		svc, err = keys.New(keys.Options{Index: st, Master: m, RetireGrace: 720 * time.Hour, Log: slog.New(slog.DiscardHandler)})
		must(t, err)
		return svc
	}})
	return e, svc
}

// TestSigned checks repomd.xml.asc: a detached signature of the bytes of repomd.xml that the
// keyring of key.asc and of repodata/repomd.xml.key (the same bytes) verifies; the same signature
// on the next GET, from index_cache; a new one after a publish, of the new repomd.xml; and one by
// the new key after a rotation, which the keyring then holds beside the retired key.
func TestSigned(t *testing.T) {
	e, svc := newSignedEnv(t)
	hello, _ := testutil.RPM(testutil.RPMOptions{})
	e.upload(hello)
	keyring := e.get("key.asc")
	if ct := keyring.Header().Get("Content-Type"); ct != keysContentType {
		t.Errorf("key.asc Content-Type %q", ct)
	}
	if b := e.get("repodata/repomd.xml.key").Body.Bytes(); !bytes.Equal(b, keyring.Body.Bytes()) {
		t.Error("repodata/repomd.xml.key is not key.asc")
	}
	sig := e.verify(keyring.Body.Bytes())
	if again := e.get("repodata/repomd.xml.asc").Body.Bytes(); !bytes.Equal(again, sig) {
		t.Error("the second GET signed again")
	}

	other, _ := testutil.RPM(testutil.RPMOptions{Name: "other"})
	e.upload(other)
	if next := e.verify(keyring.Body.Bytes()); bytes.Equal(next, sig) {
		t.Error("the signature did not change with the repomd.xml")
	}

	if _, err := svc.Rotate(t.Context(), e.repo); err != nil {
		t.Fatal(err)
	}
	rotated := e.get("key.asc").Body.Bytes()
	el, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(rotated))
	must(t, err)
	if len(el) != 2 {
		t.Fatalf("the keyring after a rotation has %d keys", len(el))
	}
	e.verify(rotated)
}

// TestUnsigned answers 404 for the signature and the keyring of a server without a master key,
// naming repo_gpgcheck=0, and serves the repodata unsigned.
func TestUnsigned(t *testing.T) {
	e := newEnv(t, envOptions{})
	for _, p := range []string{"repodata/repomd.xml.asc", "repodata/repomd.xml.key", "key.asc"} {
		if rr := e.send(http.MethodGet, p, nil); rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "repo_gpgcheck=0") {
			t.Errorf("GET %s = %d %s", p, rr.Code, rr.Body)
		}
	}
	e.get("repodata/repomd.xml")
}

// verify fetches repomd.xml and repomd.xml.asc, checks that the signature is of those bytes by a
// key of keyring, and returns it.
func (e *env) verify(keyring []byte) []byte {
	e.t.Helper()
	md := e.get("repodata/repomd.xml").Body.Bytes()
	rr := e.get("repodata/repomd.xml.asc")
	if ct := rr.Header().Get("Content-Type"); ct != signatureContentType {
		e.t.Errorf("repomd.xml.asc Content-Type %q", ct)
	}
	el, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(keyring))
	must(e.t, err)
	if _, err := openpgp.CheckArmoredDetachedSignature(el, bytes.NewReader(md), bytes.NewReader(rr.Body.Bytes()), nil); err != nil {
		e.t.Fatalf("repomd.xml.asc does not verify repomd.xml: %v", err)
	}
	return rr.Body.Bytes()
}
