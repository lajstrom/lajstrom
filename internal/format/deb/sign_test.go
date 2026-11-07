package deb

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// newKeys returns the signing service of internal/keys over st with a master key of the test.
func newKeys(t *testing.T, st index.Store) *keys.Service {
	t.Helper()
	m, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
	must(t, err)
	svc, err := keys.New(keys.Options{Index: st, Master: m, RetireGrace: 720 * time.Hour, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	return svc
}

// newSignedEnv returns an aptEnv whose routes sign with the keys service it also returns.
func newSignedEnv(t *testing.T, o envOptions) (*aptEnv, *keys.Service) {
	t.Helper()
	var svc *keys.Service
	o.signer = func(st index.Store) format.Signer {
		svc = newKeys(t, st)
		return svc
	}
	return newAptEnv(t, o), svc
}

// keyring returns the keyring the repo serves at key.asc, checking its headers.
func (e *aptEnv) keyring() openpgp.EntityList {
	e.t.Helper()
	rr := e.get("key.asc")
	if h := rr.Header(); h.Get("Content-Type") != "application/pgp-keys" || h.Get("ETag") != `"`+sha256Hex(rr.Body.Bytes())+`"` ||
		h.Get("Last-Modified") != "" {
		e.t.Errorf("key.asc headers %v, want application/pgp-keys, the ETag of the body and no Last-Modified", h)
	}
	el, err := openpgp.ReadArmoredKeyRing(rr.Body)
	must(e.t, err)
	return el
}

// signedBy returns the fingerprints of the signers of a clearsigned document or of an armored
// signature block, in order.
func signedBy(t *testing.T, doc []byte) []string {
	t.Helper()
	var body io.Reader
	if b, _ := clearsign.Decode(doc); b != nil {
		body = b.ArmoredSignature.Body
	} else {
		block, err := armor.Decode(bytes.NewReader(doc))
		must(t, err)
		body = block.Body
	}
	var fps []string
	r := packet.NewReader(body)
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return fps
		}
		must(t, err)
		if sig, ok := p.(*packet.Signature); ok {
			fps = append(fps, strings.ToUpper(hex.EncodeToString(sig.IssuerFingerprint)))
		}
	}
}

// fingerprintsOf returns the fingerprints of the keys of a keyring.
func fingerprintsOf(el openpgp.EntityList) []string {
	var fps []string
	for _, k := range el {
		fps = append(fps, strings.ToUpper(hex.EncodeToString(k.PrimaryKey.Fingerprint)))
	}
	return fps
}

// checkSigned checks the InRelease and the Release.gpg of a suite against the keyring kr: the
// cleartext of InRelease is the Release, its signature and that of Release.gpg over the
// Release bytes verify, and both have the content type of their kind.
func (e *aptEnv) checkSigned(suite string, kr openpgp.KeyRing) (release, in, sig []byte) {
	e.t.Helper()
	release = e.get("dists/" + suite + "/Release").Body.Bytes()
	inRR := e.get("dists/" + suite + "/InRelease")
	checkHeaders(e.t, inRR, "text/plain", inRR.Body.Bytes())
	in = inRR.Body.Bytes()
	b, rest := clearsign.Decode(in)
	if b == nil || len(rest) != 0 {
		e.t.Fatalf("InRelease of %s is not one clearsigned block:\n%s", suite, in)
	}
	if string(b.Plaintext)+"\n" != string(release) {
		e.t.Errorf("the cleartext of InRelease of %s is not its Release:\n%s\nwant\n%s", suite, b.Plaintext, release)
	}
	if _, err := openpgp.CheckDetachedSignature(kr, bytes.NewReader(b.Bytes), b.ArmoredSignature.Body, nil); err != nil {
		e.t.Errorf("InRelease of %s does not verify: %v", suite, err)
	}
	sigRR := e.get("dists/" + suite + "/Release.gpg")
	checkHeaders(e.t, sigRR, "application/pgp-signature", sigRR.Body.Bytes())
	sig = sigRR.Body.Bytes()
	if _, err := openpgp.CheckArmoredDetachedSignature(kr, bytes.NewReader(release), bytes.NewReader(sig), nil); err != nil {
		e.t.Errorf("Release.gpg of %s does not verify the Release: %v", suite, err)
	}
	return release, in, sig
}

// TestRoutesSigned fetches what apt fetches from a signing server: key.asc, then InRelease
// and Release.gpg of each suite, which verify against key.asc over the bytes of the Release
// served beside them; they are kept in index_cache with the RenderedAt of that Release and a
// SignedAt, served again from there, and answer HEAD and If-None-Match.
func TestRoutesSigned(t *testing.T) {
	e, svc := newSignedEnv(t, envOptions{})
	fixtureSet(t, e.pipeline)
	kr := e.keyring()
	key, err := svc.KeyFor(t.Context(), e.repo)
	must(t, err)
	if got := fingerprintsOf(kr); len(got) != 1 || got[0] != key.Key.Fingerprint {
		t.Fatalf("key.asc holds %v, want the repo's key %s", got, key)
	}
	for _, suite := range []string{"jammy", "jammy-updates"} {
		_, in, sig := e.checkSigned(suite, kr)
		if got := signedBy(t, in); len(got) != 1 || got[0] != key.Key.Fingerprint {
			t.Errorf("InRelease of %s signed by %v", suite, got)
		}
		e.checkCachedSigned(suite, map[string][]byte{kindInRelease: in, kindReleaseGPG: sig})
	}
	rr := e.do(http.MethodHead, "dists/jammy/InRelease")
	if rr.Code != http.StatusOK || rr.Body.Len() != 0 || rr.Header().Get("ETag") == "" {
		t.Errorf("HEAD InRelease = %d, %d bytes, headers %v", rr.Code, rr.Body.Len(), rr.Header())
	}
	if rr := e.do(http.MethodGet, "dists/jammy/InRelease", "If-None-Match", rr.Header().Get("ETag")); rr.Code != http.StatusNotModified {
		t.Errorf("GET InRelease with If-None-Match = %d, want 304", rr.Code)
	}
	if rr := e.do(http.MethodGet, "dists/focal/InRelease"); rr.Code != http.StatusNotFound {
		t.Errorf("GET InRelease of a suite without packages = %d, want 404", rr.Code)
	}
}

// checkCachedSigned checks the signed documents of a suite in index_cache against what the
// routes served, by kind: the same content, a SignedAt and the RenderedAt of the Release; and
// a second GET serves them again.
func (e *aptEnv) checkCachedSigned(suite string, served map[string][]byte) {
	e.t.Helper()
	rel, err := e.st.GetIndexCache(e.t.Context(), e.repo.ID, suite, kindRelease)
	must(e.t, err)
	for kind, body := range served {
		c, err := e.st.GetIndexCache(e.t.Context(), e.repo.ID, suite, kind)
		must(e.t, err)
		if !bytes.Equal(c.Content, body) || c.SignedAt == nil || !c.RenderedAt.Equal(rel.RenderedAt) {
			e.t.Errorf("index_cache %s of %s: signed at %v, rendered at %v, the Release at %v", kind, suite, c.SignedAt, c.RenderedAt, rel.RenderedAt)
		}
		if again := e.get("dists/" + suite + "/" + kind).Body.Bytes(); !bytes.Equal(again, body) {
			e.t.Errorf("the second GET of %s/%s is not the first", suite, kind)
		}
	}
}

// TestRoutesSignedFirstRequest checks that InRelease is the first request of a repo, as for
// apt: it renders the suite and makes the repo's key, and key.asc afterwards verifies it.
func TestRoutesSignedFirstRequest(t *testing.T) {
	e, _ := newSignedEnv(t, envOptions{})
	hello, _ := testutil.Deb(testutil.DebOptions{})
	e.upload(t, hello, "jammy")
	in := e.get("dists/jammy/InRelease").Body.Bytes()
	kr := e.keyring()
	b, _ := clearsign.Decode(in)
	if b == nil {
		t.Fatalf("InRelease:\n%s", in)
	}
	if _, err := openpgp.CheckDetachedSignature(kr, bytes.NewReader(b.Bytes), b.ArmoredSignature.Body, nil); err != nil {
		t.Errorf("the first InRelease against key.asc: %v", err)
	}
}

// TestRoutesSignedAfterPublish checks that a publish, which drops the repo's documents, makes
// the next InRelease the signature of the new Release.
func TestRoutesSignedAfterPublish(t *testing.T) {
	e, _ := newSignedEnv(t, envOptions{})
	fixtureSet(t, e.pipeline)
	kr := e.keyring()
	_, before, _ := e.checkSigned("jammy", kr)
	newer, _ := testutil.Deb(testutil.DebOptions{Version: "1.2.0-1"})
	e.uploadTo(t, newer, "jammy", "main")
	release, after, _ := e.checkSigned("jammy", kr)
	if bytes.Equal(before, after) {
		t.Error("InRelease did not change with a publish")
	}
	sum := listed(release)["main/binary-amd64/Packages"]
	if body := e.get("dists/jammy/main/binary-amd64/Packages").Body.Bytes(); sha256Hex(body) != sum ||
		!strings.Contains(string(body), "Version: 1.2.0-1\n") {
		t.Error("the Packages served is not the one the signed Release lists, or lacks the new version")
	}
}

// TestRoutesRotate rotates the repo's key between two updates: the next InRelease is signed by
// the new key, and in the grace by the retired one too, so it verifies against the new key
// alone, against key.asc fetched again (both keys) and against key.asc of before the
// rotation; the old InRelease verifies against the new key.asc.
func TestRoutesRotate(t *testing.T) {
	e, svc := newSignedEnv(t, envOptions{})
	fixtureSet(t, e.pipeline)
	oldRing := e.keyring()
	_, oldIn, _ := e.checkSigned("jammy", oldRing)

	rotated, err := svc.Rotate(t.Context(), e.repo)
	must(t, err)
	newRing := e.keyring()
	if got := fingerprintsOf(newRing); len(got) != 2 || got[0] != rotated.Key.Fingerprint || got[1] != fingerprintsOf(oldRing)[0] {
		t.Fatalf("key.asc after the rotation holds %v, want the new key %s and the retired one", got, rotated)
	}
	_, in, sig := e.checkSigned("jammy", openpgp.EntityList{rotated.OpenPGP})
	if bytes.Equal(in, oldIn) {
		t.Fatal("InRelease is the same after the rotation")
	}
	want := []string{rotated.Key.Fingerprint, fingerprintsOf(oldRing)[0]}
	for name, doc := range map[string][]byte{"InRelease": in, "Release.gpg": sig} {
		if got := signedBy(t, doc); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s after the rotation signed by %v, want %v", name, got, want)
		}
	}
	e.checkSigned("jammy", newRing)
	e.checkSigned("jammy", oldRing)
	b, _ := clearsign.Decode(oldIn)
	if _, err := openpgp.CheckDetachedSignature(newRing, bytes.NewReader(b.Bytes), b.ArmoredSignature.Body, nil); err != nil {
		t.Errorf("the InRelease of before the rotation against the new key.asc: %v", err)
	}
}

// TestRoutesSignedAuthorize checks that the signed documents and key.asc need read like the
// rest: 401 to the anonymous principal, 404 to another user, and the documents to alice.
func TestRoutesSignedAuthorize(t *testing.T) {
	e, _ := newSignedEnv(t, envOptions{authz: authorizer{}})
	fixtureSet(t, e.pipeline)
	for _, path := range []string{"dists/jammy/InRelease", "dists/jammy/Release.gpg", "key.asc"} {
		if rr := e.do(http.MethodGet, path); rr.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d, want 401", path, rr.Code)
		}
		if rr := e.do(http.MethodGet, path, "X-Test-User", "bob"); rr.Code != http.StatusNotFound {
			t.Errorf("bob's GET %s = %d, want 404", path, rr.Code)
		}
		e.get(path, "X-Test-User", "alice")
	}
}

// stubSigner wraps a Signer: fail names the method that fails with errBoom, and before runs at
// the start of every ClearSign.
type stubSigner struct {
	format.Signer
	fail   string
	before func()
}

func (s stubSigner) ClearSign(ctx context.Context, repo index.Repo, data []byte) ([]byte, error) {
	if s.before != nil {
		s.before()
	}
	if s.fail == "ClearSign" {
		return nil, errBoom
	}
	return s.Signer.ClearSign(ctx, repo, data)
}

func (s stubSigner) DetachSign(ctx context.Context, repo index.Repo, data []byte) ([]byte, error) {
	if s.fail == "DetachSign" {
		return nil, errBoom
	}
	return s.Signer.DetachSign(ctx, repo, data)
}

func (s stubSigner) PublicKeys(ctx context.Context, repo index.Repo) ([]byte, error) {
	if s.fail == "PublicKeys" {
		return nil, errBoom
	}
	return s.Signer.PublicKeys(ctx, repo)
}

// TestRoutesSignFails checks that a Signer that fails is 500, its cause in the log only.
func TestRoutesSignFails(t *testing.T) {
	for _, tt := range []struct{ fail, path string }{
		{"ClearSign", "dists/jammy/InRelease"},
		{"DetachSign", "dists/jammy/Release.gpg"},
		{"PublicKeys", "key.asc"},
	} {
		t.Run(tt.fail, func(t *testing.T) {
			e := newAptEnv(t, envOptions{signer: func(st index.Store) format.Signer {
				return stubSigner{Signer: newKeys(t, st), fail: tt.fail}
			}})
			fixtureSet(t, e.pipeline)
			if rr := e.do(http.MethodGet, tt.path); rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "boom") {
				t.Errorf("GET %s = %d %q, want 500 without the cause", tt.path, rr.Code, rr.Body)
			}
		})
	}
}

// TestRoutesSignedRenderedAgain checks signing while the suite is rendered again: an
// invalidation, or an invalidation and a render, between the read of the Release and the store
// of its signatures makes signed sign again, so that what it stores is the signature of the
// Release in index_cache; a suite invalidated on every round is 500; and a signed document of
// an older render is not served.
func TestRoutesSignedRenderedAgain(t *testing.T) {
	var st index.Store
	var p *pipeline
	rounds := 0
	rerender := func(times int, render bool) func() {
		return func() {
			if rounds++; rounds > times {
				return
			}
			must(t, st.InvalidateIndexCache(t.Context(), p.repo.ID, ""))
			if render {
				_, err := format.CachedDocument(t.Context(), st, Plugin{svc: &services{providers: provider.Map{"local": p.prov}}},
					p.repo, "jammy", kindRelease)
				must(t, err)
			}
		}
	}
	for _, tt := range []struct {
		name   string
		times  int  // invalidations in the way of signed
		render bool // and a render after each
		want   int
	}{
		{"invalidated once", 1, false, http.StatusOK},
		{"rendered again once", 1, true, http.StatusOK},
		{"invalidated every round", signRounds, false, http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rounds = 0
			e := newAptEnv(t, envOptions{signer: func(s index.Store) format.Signer {
				st = s
				return stubSigner{Signer: newKeys(t, s), before: rerender(tt.times, tt.render)}
			}})
			p = e.pipeline
			fixtureSet(t, e.pipeline)
			rr := e.do(http.MethodGet, "dists/jammy/InRelease")
			if rr.Code != tt.want {
				t.Fatalf("GET InRelease = %d %s, want %d", rr.Code, rr.Body, tt.want)
			}
			if tt.want != http.StatusOK {
				return
			}
			b, _ := clearsign.Decode(rr.Body.Bytes())
			if release := e.get("dists/jammy/Release").Body.String(); b == nil || string(b.Plaintext)+"\n" != release {
				t.Error("InRelease does not sign the Release in index_cache")
			}
		})
	}

	// A signed document whose RenderedAt is not that of the Release is of an older render.
	e, _ := newSignedEnv(t, envOptions{})
	fixtureSet(t, e.pipeline)
	stale := []byte("an InRelease of an older render")
	must(t, e.st.PutIndexCache(t.Context(), &index.Cache{RepoID: e.repo.ID, Scope: "jammy", Kind: kindInRelease, Content: stale,
		SHA256: sha256Hex(stale), RenderedAt: renderedAt}))
	if got := e.get("dists/jammy/InRelease").Body.Bytes(); bytes.Equal(got, stale) {
		t.Error("the InRelease of an older render is served")
	}
}

// failingIndex fails GetIndexCache of the kind get outside a transaction, and PutIndexCache of
// the kind put in one.
type failingIndex struct {
	index.Store
	get, put string
}

func (s failingIndex) GetIndexCache(ctx context.Context, repoID int64, scope, kind string) (*index.Cache, error) {
	if kind == s.get {
		return nil, errBoom
	}
	return s.Store.GetIndexCache(ctx, repoID, scope, kind)
}

func (s failingIndex) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.Store.Tx(ctx, func(w index.Writer) error { return fn(putFailingTx{Writer: w, put: s.put}) })
}

// putFailingTx is a transaction of failingIndex.
type putFailingTx struct {
	index.Writer
	put string
}

func (w putFailingTx) PutIndexCache(ctx context.Context, c *index.Cache) error {
	if c.Kind == w.put {
		return errBoom
	}
	return w.Writer.PutIndexCache(ctx, c)
}

// TestRoutesSignedIndexFails checks that an error of the index on the read or the store of a
// signed document is the error of signed.
func TestRoutesSignedIndexFails(t *testing.T) {
	e, _ := newSignedEnv(t, envOptions{})
	fixtureSet(t, e.pipeline)
	for _, st := range []failingIndex{{Store: e.st, get: kindInRelease}, {Store: e.st, put: kindReleaseGPG}} {
		h := &handlers{deps: format.Deps{Index: st, Signer: newKeys(t, e.st), Log: slog.New(slog.DiscardHandler)},
			plugin: fixedClock(e.pipeline)}
		if _, err := h.signed(t.Context(), e.repo, "jammy", kindInRelease); !errors.Is(err, errBoom) {
			t.Errorf("signed with %+v failing: %v, want errBoom", st, err)
		}
	}
}
