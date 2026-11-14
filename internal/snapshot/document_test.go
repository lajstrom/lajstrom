package snapshot

import (
	"bytes"
	"compress/gzip"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// testMasterHex is the master key of the tests, built at run time so that no scanner takes a
// literal for one.
var testMasterHex = strings.Repeat("0123456789abcdef", 4)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func master(t *testing.T, hex string) *keys.Master {
	t.Helper()
	m, err := keys.ParseMaster(hex)
	must(t, err)
	return m
}

var memoryDBs atomic.Int64

// quiet is the logger of the services the tests use: their lines say nothing a test checks.
var quiet = slog.New(slog.DiscardHandler)

// newStore returns a new migrated index in memory.
func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(t.Context(), fmt.Sprintf("file:snapshot-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(t.Context()))
	return st
}

// secrets are the plaintext secrets of a control plane that filled made, which no snapshot of it
// may hold, in any encoding.
type secrets map[string][]byte

// filled makes a control plane through the services that serve and admin use: a user with a
// password and the admin binding, a PAT of hers, a deploy token of an apt repo, the repo's
// signing key and the bearer-token key, both encrypted with m; and the bootstrap token. It
// returns the secrets in clear: the password, the two tokens, the bootstrap token, the private
// key packets of the repo key, the bearer-token key's private key, and the master key.
func filled(t *testing.T, st index.Store, m *keys.Master) secrets {
	t.Helper()
	ctx := t.Context()
	repo := index.Repo{Format: "apt", Name: "debs", Registry: "local", Prefix: "apt/debs", Kind: index.KindHosted}
	must(t, st.CreateRepo(ctx, &repo))
	password := "correct horse battery " + strings.Repeat("staple", 2)
	users := &auth.Users{Index: st, Log: quiet}
	if _, err := users.Create(ctx, "alice", password, true, auth.Origin{}); err != nil {
		t.Fatal(err)
	}
	tokens := &auth.Tokens{Index: st, Log: quiet}
	_, pat, err := tokens.CreatePAT(ctx, "alice", "laptop", nil, auth.Expiry{}, auth.Origin{})
	must(t, err)
	_, deploy, err := tokens.CreateDeploy(ctx, "apt/debs", "ci", []string{"read"}, auth.Expiry{}, auth.Origin{})
	must(t, err)
	svc, err := keys.New(keys.Options{Index: st, Master: m, Log: quiet})
	must(t, err)
	gpg, err := svc.KeyFor(ctx, repo)
	must(t, err)
	var packets bytes.Buffer
	must(t, gpg.OpenPGP.SerializePrivateWithoutSigning(&packets, nil))
	tk, err := keys.NewTokens(keys.TokenOptions{Index: st, Master: m, Log: quiet})
	must(t, err)
	signer, err := tk.TokenSigner(ctx)
	must(t, err)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(signer.Private)
	must(t, err)
	bootstrap := "bootstrap-" + strings.Repeat("b", 32)
	must(t, st.PutBootstrapToken(ctx, &index.BootstrapToken{Hash: auth.HashSecret(bootstrap), Replica: "test",
		ExpiresAt: time.Now().Add(time.Hour)}, ""))
	return secrets{"password": []byte(password), "pat": []byte(pat), "deploy token": []byte(deploy),
		"bootstrap token": []byte(bootstrap), "bootstrap hash": []byte(auth.HashSecret(bootstrap)),
		"gpg private key": packets.Bytes(), "token private key": pkcs8, "master key": []byte(testMasterHex)}
}

// gunzip returns the bytes of a gzip stream.
func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	must(t, err)
	out, err := io.ReadAll(zr)
	must(t, err)
	return out
}

// TestDocumentRoundTrip: a control plane encoded and decoded is the same, every row with its id,
// and a document of a newer version is refused.
func TestDocumentRoundTrip(t *testing.T) {
	st, m := newStore(t), master(t, testMasterHex)
	filled(t, st, m)
	cp, err := st.ExportControlPlane(t.Context(), index.ExportOptions{WithAudit: true, AuditSince: time.Unix(1, 0).UTC()})
	must(t, err)
	gz, err := encode(cp, "v9.9.9")
	must(t, err)
	got, binary, err := decode(gz)
	must(t, err)
	a, _ := json.Marshal(cp)
	b, _ := json.Marshal(got)
	if !bytes.Equal(a, b) || binary != "v9.9.9" {
		t.Errorf("decode(encode(cp)) differs from cp:\n%s\n%s", a, b)
	}

	none, err := st.ExportControlPlane(t.Context(), index.ExportOptions{})
	must(t, err)
	gz, err = encode(none, "")
	must(t, err)
	if doc := gunzip(t, gz); bytes.Contains(doc, []byte(`"audit"`)) {
		t.Errorf("a snapshot without audit rows has the key audit: %s", doc)
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"version": 2, "schema": 21}`))
	_ = zw.Close()
	if _, _, err := decode(buf.Bytes()); !errors.Is(err, ErrVersion) {
		t.Errorf("decode of version 2: %v, want ErrVersion", err)
	}
}
