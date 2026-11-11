package keys

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// newTokens returns the Tokens of o over the env's database, master key and clock.
func (e *env) newTokens(o TokenOptions) *Tokens {
	e.t.Helper()
	if o.Index == nil {
		o.Index = e.st
	}
	if o.Master == nil {
		o.Master = e.svc.master
	}
	if o.Now == nil {
		o.Now = func() time.Time { return e.now }
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	t, err := NewTokens(o)
	must(e.t, err)
	return t
}

// fingerprints of token keys, in order.
func tokenIDs(ks []*TokenKey) []string {
	var ids []string
	for _, k := range ks {
		ids = append(ids, k.ID())
	}
	return ids
}

func TestNewTokens(t *testing.T) {
	st, m := &struct{ index.Store }{}, master(t, testMasterHex)
	for _, tt := range []struct {
		name string
		o    TokenOptions
		want string
	}{
		{"no index", TokenOptions{Master: m}, "no index"},
		{"no master key", TokenOptions{Index: st}, "no master key"},
		{"an unknown algorithm", TokenOptions{Index: st, Master: m, Algorithm: "HS256"}, `token algorithm "HS256" is not ES256 or EdDSA`},
	} {
		if _, err := NewTokens(tt.o); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("NewTokens with %s: %v, want an error containing %q", tt.name, err, tt.want)
		}
	}
	tk, err := NewTokens(TokenOptions{Index: st, Master: m})
	must(t, err)
	if tk.algorithm != TokenES256 || tk.now == nil || tk.log == nil {
		t.Errorf("NewTokens with defaults = %+v", tk)
	}
	if TokenGrace != 21*time.Minute {
		t.Errorf("the grace is %v, want the lifetime and the skew, 21 minutes", TokenGrace)
	}
}

// TestTokenSigner checks the token key made on first use, for both algorithms: one row of kind
// jwt without a repo, the PKIX public key, the fingerprint its lowercase hexadecimal sha256,
// the private key sealed (no PKCS #8 in the clear) and opened by the master key; the next call
// returns the same key, decrypted from the row; ExportToken prints its public key.
func TestTokenSigner(t *testing.T) {
	for _, tt := range []struct {
		alg string
		pub func(crypto.PublicKey) bool
	}{
		{TokenES256, func(p crypto.PublicKey) bool { k, ok := p.(*ecdsa.PublicKey); return ok && k.Curve == elliptic.P256() }},
		{TokenEdDSA, func(p crypto.PublicKey) bool { _, ok := p.(ed25519.PublicKey); return ok }},
	} {
		t.Run(tt.alg, func(t *testing.T) {
			e := newEnv(t, Options{})
			tk := e.newTokens(TokenOptions{Algorithm: tt.alg})
			ctx := t.Context()
			k, err := tk.TokenSigner(ctx)
			must(t, err)
			rows := must2(e.st.ListKeys(ctx, nil))
			if len(rows) != 1 {
				t.Fatalf("%d server keys, want 1", len(rows))
			}
			row := rows[0]
			checkTokenRow(t, row, k, tt.alg, tt.pub, e.now)
			again, err := tk.TokenSigner(ctx)
			must(t, err)
			if again.ID() != k.ID() || again.Key.ID != row.ID {
				t.Errorf("the second call returned %s, want %s", again, k)
			}
			b, err := tk.ExportToken(ctx)
			must(t, err)
			block, rest := pem.Decode(b)
			if block == nil || block.Type != "PUBLIC KEY" || !bytes.Equal(block.Bytes, row.Public) || len(rest) != 0 {
				t.Errorf("export %q", b)
			}
		})
	}
}

// checkTokenRow checks the row of the key k of alg, made at now: kind jwt, no repo, active, the
// fingerprint the sha256 of the PKIX public key, the private key sealed.
func checkTokenRow(t *testing.T, row index.Key, k *TokenKey, alg string, pubOK func(crypto.PublicKey) bool, now time.Time) {
	t.Helper()
	sum := sha256.Sum256(row.Public)
	switch {
	case row.Kind != KindJWT || row.RepoID != nil || row.RetiredAt != nil:
		t.Errorf("row %s, repo %v, retired %v", row.Kind, row.RepoID, row.RetiredAt)
	case row.Fingerprint != hex.EncodeToString(sum[:]) || k.ID() != row.Fingerprint:
		t.Errorf("fingerprint %s, key %s, want the sha256 of the public key", row.Fingerprint, k.ID())
	case k.Algorithm != alg || !pubOK(k.Public) || k.Private == nil:
		t.Errorf("key %s: %T, private %T", k.Algorithm, k.Public, k.Private)
	case !row.CreatedAt.Equal(now):
		t.Errorf("created at %v, want %v", row.CreatedAt, now)
	}
	if pub, err := x509.ParsePKIXPublicKey(row.Public); err != nil || !pubOK(pub) {
		t.Errorf("the public column: %T, %v", pub, err)
	}
	if _, err := x509.ParsePKCS8PrivateKey(row.PrivateEnc); err == nil || bytes.Contains(row.PrivateEnc, row.Public) {
		t.Error("the private key is stored in the clear")
	}
}

// TestTokenSignerRace runs two TokenSigner at once without a key: both make one, the unique
// index keeps one, and both return it; one row.
func TestTokenSignerRace(t *testing.T) {
	e := newEnv(t, Options{})
	race := raceStore{Store: e.st, calls: &atomic.Int32{}, both: make(chan struct{})}
	tk := e.newTokens(TokenOptions{Index: race})
	var wg sync.WaitGroup
	got := make([]*TokenKey, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() { got[i], errs[i] = tk.TokenSigner(t.Context()) })
	}
	wg.Wait()
	must(t, errors.Join(errs...))
	if got[0].ID() != got[1].ID() {
		t.Errorf("the two TokenSigner returned %s and %s", got[0], got[1])
	}
	rows := must2(e.st.ListKeys(t.Context(), nil))
	if len(rows) != 1 || race.calls.Load() != 3 {
		t.Errorf("%d keys and %d GetKey calls, want 1 key and 3 calls (the loser reads the winner's key)", len(rows), race.calls.Load())
	}
}

// TestRotateToken checks a rotation: the new key signs, both verify for the grace (the new one
// first) and the old one not after it; the audit row; the keys of TokenVerifiers have no
// private key. A rotation with drop: the old key verifies nothing from then on, and neither
// does the key of the rotation before, still in its grace: every outstanding token dies.
func TestRotateToken(t *testing.T) {
	e := newEnv(t, Options{})
	tk := e.newTokens(TokenOptions{})
	ctx := auth.WithPrincipal(t.Context(), auth.Principal{Kind: auth.KindUser, Name: "admin"})
	if vs := must2(tk.TokenVerifiers(ctx)); len(vs) != 0 {
		t.Errorf("verifiers without a key: %v", vs)
	}
	first := must2(tk.TokenSigner(ctx))
	second := must2(tk.RotateToken(ctx, false))
	if second.ID() == first.ID() || second.Key.ID == 0 || second.Private == nil {
		t.Fatalf("rotated to %s (id %d) from %s", second, second.Key.ID, first)
	}
	if signer := must2(tk.TokenSigner(ctx)); signer.ID() != second.ID() {
		t.Errorf("signs with %s after the rotation, want %s", signer, second)
	}
	for _, tt := range []struct {
		after time.Duration
		want  []string
	}{
		{0, []string{second.ID(), first.ID()}},
		{TokenGrace - time.Second, []string{second.ID(), first.ID()}},
		{TokenGrace, []string{second.ID()}},
	} {
		e.now = e.now.Add(tt.after)
		vs := must2(tk.TokenVerifiers(ctx))
		if !equal(tokenIDs(vs), tt.want) {
			t.Errorf("%v after the rotation: verifiers %v, want %v", tt.after, tokenIDs(vs), tt.want)
		}
		for _, v := range vs {
			if v.Private != nil {
				t.Errorf("verifier %s has its private key", v)
			}
		}
		e.now = e.now.Add(-tt.after)
	}

	third := must2(tk.RotateToken(ctx, true))
	if vs := must2(tk.TokenVerifiers(ctx)); !equal(tokenIDs(vs), []string{third.ID()}) {
		t.Errorf("after a rotation with drop: verifiers %v, want the new key only, the first in its grace included", tokenIDs(vs))
	}
	for _, r := range must2(e.st.ListKeys(ctx, nil)) {
		if r.ID != third.Key.ID && (r.RetiredAt == nil || !r.RetiredAt.Equal(e.now.Add(-TokenGrace))) {
			t.Errorf("key %s after the drop: retired at %v, want one grace ago", r.Fingerprint, r.RetiredAt)
		}
	}
	checkRotateAudit(t, e, []string{third.ID(), `{"retired":"` + second.ID() + `","dropped":true}`},
		[]string{second.ID(), `{"retired":"` + first.ID() + `"}`})
}

// checkRotateAudit checks the key.rotate rows, the newest first, each a subject and its
// details, by user:admin and without a repo.
func checkRotateAudit(t *testing.T, e *env, want ...[]string) {
	t.Helper()
	evs := must2(e.st.ListAudit(t.Context(), index.AuditFilter{Action: ActionRotate}))
	if len(evs) != len(want) {
		t.Fatalf("audit rows %+v", evs)
	}
	for i, ev := range evs {
		if ev.Subject != want[i][0] || ev.Repo != "" || ev.Principal != "user:admin" || !jsonEqual(ev.Details, want[i][1]) {
			t.Errorf("audit row %+v, want %s %s", ev, want[i][0], want[i][1])
		}
	}
}

// TestRotateTokenFirst checks a rotation without a key: it makes the first, with no retired
// key in the audit row.
func TestRotateTokenFirst(t *testing.T) {
	e := newEnv(t, Options{})
	k := must2(e.newTokens(TokenOptions{Algorithm: TokenEdDSA}).RotateToken(t.Context(), false))
	evs := must2(e.st.ListAudit(t.Context(), index.AuditFilter{Action: ActionRotate}))
	if len(evs) != 1 || evs[0].Subject != k.ID() || evs[0].Details != nil || k.Algorithm != TokenEdDSA {
		t.Errorf("audit rows %+v, key %s", evs, k.Algorithm)
	}
}

// TestDropToken checks the drop: the active key retired one grace ago, so that it verifies
// nothing, its audit row, a second drop with nothing to drop, and the next TokenSigner making a
// new key.
func TestDropToken(t *testing.T) {
	e := newEnv(t, Options{})
	tk := e.newTokens(TokenOptions{})
	ctx := t.Context()
	k := must2(tk.TokenSigner(ctx))
	fp, err := tk.DropToken(ctx)
	if err != nil || fp != k.ID() {
		t.Fatalf("DropToken: %q, %v; want %s", fp, err, k.ID())
	}
	rows := must2(e.st.ListKeys(ctx, nil))
	if len(rows) != 1 || rows[0].RetiredAt == nil || !rows[0].RetiredAt.Equal(e.now.Add(-TokenGrace)) {
		t.Errorf("rows after the drop %+v", rows)
	}
	if vs := must2(tk.TokenVerifiers(ctx)); len(vs) != 0 {
		t.Errorf("verifiers after the drop: %v", tokenIDs(vs))
	}
	if _, err := tk.DropToken(ctx); !errors.Is(err, ErrNoTokenKey) {
		t.Errorf("a second drop: %v, want ErrNoTokenKey", err)
	}
	evs := must2(e.st.ListAudit(ctx, index.AuditFilter{Action: ActionDrop}))
	if len(evs) != 1 || evs[0].Subject != k.ID() || evs[0].Details != nil {
		t.Errorf("audit rows %+v", evs)
	}
	if next := must2(tk.TokenSigner(ctx)); next.ID() == k.ID() {
		t.Error("TokenSigner after the drop signs with the dropped key")
	}
}

// TestTokenUndecryptable checks keys that do not decrypt: an active key made under another
// master key is an error of TokenSigner, TokenVerifiers and ExportToken that names it and the
// way out; a retired one is left out of the verifiers with a warning.
func TestTokenUndecryptable(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := t.Context()
	k := must2(e.newTokens(TokenOptions{}).TokenSigner(ctx))
	var logged bytes.Buffer
	other := e.newTokens(TokenOptions{Master: master(t, otherMasterHex), Log: slog.New(slog.NewTextHandler(&logged, nil))})
	want := "keys: the private key of " + k.ID() + " does not decrypt with the master key: another master key, or the row " +
		"was changed; start with the master key that made it, or retire it with lajstrom admin key drop --token, and a new key is made"
	for name, call := range map[string]func() error{
		"TokenSigner":    func() error { _, err := other.TokenSigner(ctx); return err },
		"TokenVerifiers": func() error { _, err := other.TokenVerifiers(ctx); return err },
		"ExportToken":    func() error { _, err := other.ExportToken(ctx); return err },
	} {
		if err := call(); err == nil || err.Error() != want {
			t.Errorf("%s with another master key: %v", name, err)
		}
	}
	must(t, e.st.RetireKey(ctx, k.Key.ID, e.now)) // retired within the grace, under the old master key
	made := must2(other.TokenSigner(ctx))
	vs := must2(other.TokenVerifiers(ctx))
	if !equal(tokenIDs(vs), []string{made.ID()}) {
		t.Errorf("verifiers %v, want the new key alone", tokenIDs(vs))
	}
	if !strings.Contains(logged.String(), `level=WARN msg="retired token key left out: it does not decrypt" fingerprint=`+k.ID()) {
		t.Errorf("no warning:\n%s", logged.String())
	}
}

// TestTokenRowsTampered checks open on rows that a Lajstrom holding the master key did not
// write: another fingerprint, the sealed private key of another key, another public key, no
// key, a key on another curve, an RSA key.
func TestTokenRowsTampered(t *testing.T) {
	e := newEnv(t, Options{})
	tk := e.newTokens(TokenOptions{})
	row, _, err := tk.generate()
	must(t, err)
	other, _, err := tk.generate()
	must(t, err)
	seal := func(k index.Key, plain []byte) index.Key {
		k.PrivateEnc = tk.master.seal(k.Fingerprint, plain)
		return k
	}
	pkcs8 := func(priv any) []byte {
		b, err := x509.MarshalPKCS8PrivateKey(priv)
		must(t, err)
		return b
	}
	p384 := must2(ecdsa.GenerateKey(elliptic.P384(), rand.Reader))
	rsaKey := must2(rsa.GenerateKey(rand.Reader, 2048))
	otherPlain := must2(tk.master.open(other.Fingerprint, other.PrivateEnc))
	changedPublic := *row
	changedPublic.Public = other.Public
	renamed := *row
	renamed.Fingerprint = other.Fingerprint
	for name, tt := range map[string]struct {
		row  index.Key
		want string
	}{
		"another fingerprint":            {renamed, "does not decrypt with the master key"},
		"the private key of another key": {seal(*row, otherPlain), "holds the key " + other.Fingerprint},
		"another public key":             {changedPublic, "holds another public key"},
		"no key":                         {seal(*row, []byte("not a key")), "keys: the private key of " + row.Fingerprint},
		"a P-384 key":                    {seal(*row, pkcs8(p384)), "an ECDSA key on P-384 is not a token key"},
		"an RSA key":                     {seal(*row, pkcs8(rsaKey)), "a *rsa.PrivateKey is not a token key"},
	} {
		if _, err := tk.open(tt.row); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
	if k, err := tk.open(*row); err != nil || k.ID() != row.Fingerprint {
		t.Errorf("the row itself: %v, %v", k, err)
	}
}

// TestTokenStoreFails checks that an error of the index reaches the caller, wrapped, from every
// call, and that a rotation that loses to another is index.ErrConflict and changes nothing.
func TestTokenStoreFails(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := t.Context()
	failing := func(call string) *Tokens {
		return e.newTokens(TokenOptions{Index: failStore{Store: e.st, fail: map[string]bool{call: true}}})
	}
	if _, err := failing("CreateKey").TokenSigner(ctx); !errors.Is(err, errBoom) { // no key yet
		t.Errorf("with CreateKey failing: %v, want it wrapped", err)
	}
	before := must2(e.newTokens(TokenOptions{}).TokenSigner(ctx))
	for _, tt := range []struct {
		fail string
		call func(*Tokens) error
	}{
		{"GetKey", func(tk *Tokens) error { _, err := tk.TokenSigner(ctx); return err }},
		{"ListKeys", func(tk *Tokens) error { _, err := tk.TokenVerifiers(ctx); return err }},
		{"tx.GetKey", func(tk *Tokens) error { _, err := tk.RotateToken(ctx, false); return err }},
		{"tx.AppendAudit", func(tk *Tokens) error { _, err := tk.RotateToken(ctx, true); return err }},
		{"tx.GetKey", func(tk *Tokens) error { _, err := tk.DropToken(ctx); return err }},
		{"tx.AppendAudit", func(tk *Tokens) error { _, err := tk.DropToken(ctx); return err }},
	} {
		if err := tt.call(failing(tt.fail)); !errors.Is(err, errBoom) {
			t.Errorf("with %s failing: %v, want it wrapped", tt.fail, err)
		}
	}
	tk := e.newTokens(TokenOptions{Index: conflictStore{e.st}})
	if _, err := tk.RotateToken(ctx, false); !errors.Is(err, index.ErrConflict) {
		t.Errorf("a rotation that loses: %v, want index.ErrConflict", err)
	}
	after := must2(e.newTokens(TokenOptions{}).TokenSigner(ctx))
	if after.Key.ID != before.Key.ID || len(must2(e.st.ListKeys(ctx, nil))) != 1 {
		t.Errorf("after failed rotations and drops: the key %s, want %s alone", after, before)
	}
}

// TestEphemeralTokens checks the key of a server without a master key: ECDSA P-256, no row, the
// same key to sign and to verify, the verifier without its private key; each one is another key.
func TestEphemeralTokens(t *testing.T) {
	e := NewEphemeralTokens()
	s := must2(e.TokenSigner(t.Context()))
	vs := must2(e.TokenVerifiers(t.Context()))
	sum := sha256.Sum256(s.Key.Public)
	switch {
	case s.Algorithm != TokenES256 || s.Private == nil || s.Key.ID != 0 || s.Key.PrivateEnc != nil || s.ID() != hex.EncodeToString(sum[:]):
		t.Errorf("the ephemeral signer %s: %s, private %T, id %d", s, s.Algorithm, s.Private, s.Key.ID)
	case len(vs) != 1 || vs[0].ID() != s.ID() || vs[0].Private != nil:
		t.Errorf("the ephemeral verifiers %v", vs)
	}
	if again := must2(NewEphemeralTokens().TokenSigner(t.Context())); again.ID() == s.ID() {
		t.Error("two ephemeral keys are the same")
	}
}

// TestTokenKeyRedacted checks that a token key prints as its fingerprint, in every verb and in
// a log line, never with its private key.
func TestTokenKeyRedacted(t *testing.T) {
	e := newEnv(t, Options{})
	for _, alg := range []string{TokenES256, TokenEdDSA} {
		k := must2(e.newTokens(TokenOptions{Algorithm: alg}).RotateToken(t.Context(), false))
		var secret string
		switch p := k.Private.(type) {
		case *ecdsa.PrivateKey:
			b, err := p.Bytes()
			must(t, err)
			secret = hex.EncodeToString(b)
		case ed25519.PrivateKey:
			secret = hex.EncodeToString(p.Seed())
		}
		var logged bytes.Buffer
		slog.New(slog.NewJSONHandler(&logged, nil)).Info("key", "key", k)
		for _, s := range []string{fmt.Sprint(k), fmt.Sprintf("%v", k), fmt.Sprintf("%+v", k), fmt.Sprintf("%#v", k),
			fmt.Sprint([]*TokenKey{k}), logged.String()} {
			if !strings.Contains(s, k.ID()) || strings.Contains(s, secret) || strings.Contains(s, "Private") {
				t.Errorf("%s: a key prints as %q", alg, s)
			}
		}
	}
}

// TestTokenRetiredOrder checks the order of the verifiers: the active key, then the retired
// ones, the most recently retired first.
func TestTokenRetiredOrder(t *testing.T) {
	e := newEnv(t, Options{})
	tk := e.newTokens(TokenOptions{})
	ctx := t.Context()
	var made []string
	for range 3 {
		made = append(made, must2(tk.RotateToken(ctx, false)).ID())
		e.now = e.now.Add(time.Minute)
	}
	if vs := must2(tk.TokenVerifiers(ctx)); !equal(tokenIDs(vs), []string{made[2], made[1], made[0]}) {
		t.Errorf("verifiers %v, want %v reversed", tokenIDs(vs), made)
	}
}
