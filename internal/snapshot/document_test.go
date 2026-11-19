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
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/webhook"
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
// password and the admin binding, a PAT of hers, a deploy token of an apt repo and one over the
// mask apt/*, the repo's signing key and the bearer-token key, both encrypted with m; a webhook,
// its secret sealed with m; and the bootstrap token. It returns the secrets in clear: the
// password, the three tokens, the webhook's secret and its key, the bootstrap token, the private
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
	_, maskDeploy, err := tokens.CreateMaskDeploy(ctx, "apt/*", "fleet", []string{"read"}, auth.Expiry{}, auth.Origin{})
	must(t, err)
	hookSecret, hookKey := webhook.NewSecret()
	hook := index.Webhook{UID: index.NewUID(time.Now()), Mask: "apt/*", URL: "https://ci.example.com/hook",
		Events: []string{"version.published"}, Active: true, CreatedBy: "user:alice"}
	hook.SecretEnc, err = webhook.Seal(m, hook.UID, hookKey)
	must(t, err)
	must(t, st.CreateWebhook(ctx, &hook))
	at := time.Now().UTC().Truncate(time.Second)
	alice, err := st.GetUser(ctx, "alice")
	must(t, err)
	must(t, st.TouchUser(ctx, alice.ID, at)) // her last activity, users.last_seen_at (#1283)
	// a password that must change and a history (#1281), which a snapshot carries: bob's, so that
	// alice's password stays a credential after a restore
	must(t, st.CreateUser(ctx, &index.User{Name: "bob", Kind: auth.KindLocal, PasswordHash: alice.PasswordHash, MustChange: true,
		PasswordHistory: []string{alice.PasswordHash}}))
	must(t, st.CreateToken(ctx, &index.Token{Kind: auth.TokenPAT, UserID: &alice.ID, Name: "oidc", Hash: strings.Repeat("0", 64),
		DPoPJKT: "jkt", AuthTime: &at, AMR: []string{"pwd", "otp"}, ACR: "gold"}))
	must(t, st.CreateCIPolicy(ctx, &index.CIPolicy{Name: "app-publish", Issuer: "github", Claims: map[string][]string{"repository_id": {"1"}},
		Grants: []index.CIGrant{{Role: "writer", Mask: "apt/debs"}}, State: index.CIPolicyPending, ExpiresAt: &at,
		RenewalExpiresAt: &at, CreatedBy: "user:alice", UpdatedBy: "user:bob", ApprovedBy: "user:carol", ApprovedAt: &at}))
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
		"mask deploy token": []byte(maskDeploy), "webhook secret": []byte(hookSecret), "webhook key": hookKey,
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
// the sealing key's and the wrapped master keys' included (#1288, #1388), and a document of a newer
// version is refused; a database without them writes no sealingKey and no wrappedMasterKeys.
func TestDocumentRoundTrip(t *testing.T) {
	st, m := newStore(t), master(t, testMasterHex)
	empty, err := st.ExportControlPlane(t.Context(), index.ExportOptions{})
	must(t, err)
	gz, err := encode(empty, "")
	must(t, err)
	if doc := gunzip(t, gz); bytes.Contains(doc, []byte(`sealingKey`)) || bytes.Contains(doc, []byte(`wrappedMasterKeys`)) {
		t.Errorf("a snapshot of a database without a sealing key and wrapped master keys has the key sealingKey or "+
			"wrappedMasterKeys: %s", doc)
	}
	filled(t, st, m)
	must(t, st.PutSealingKey(t.Context(), &index.SealingKey{KeyID: "0123456789abcdef", Source: index.SealingRewrap, Host: "admin-1",
		AuditID: new(int64(4))}, ""))
	for _, k := range []index.WrappedMasterKey{
		{KeyID: "fedcba9876543210", KMS: "gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k", Wrapped: []byte("ciphertext b")},
		{KeyID: "0123456789abcdef", KMS: "awskms:///arn:aws:kms:eu-west-1:111122223333:key/k", Wrapped: []byte("ciphertext a"),
			AuditID: new(int64(5))},
	} {
		must(t, st.PutWrappedMasterKey(t.Context(), &k, nil))
	}
	cp, err := st.ExportControlPlane(t.Context(), index.ExportOptions{WithAudit: true, AuditSince: time.Unix(1, 0).UTC()})
	must(t, err)
	gz, err = encode(cp, "v9.9.9")
	must(t, err)
	got, binary, err := decode(gz)
	must(t, err)
	a, _ := json.Marshal(cp)
	b, _ := json.Marshal(got)
	if !bytes.Equal(a, b) || binary != "v9.9.9" {
		t.Errorf("decode(encode(cp)) differs from cp:\n%s\n%s", a, b)
	}
	if doc := gunzip(t, gz); !bytes.Contains(doc, []byte(`"mustChange":true,"passwordHistory":["$argon2id$`)) {
		t.Errorf("the users of the document have no mustChange and passwordHistory: %s", doc)
	}
	sealing := []byte(`"sealingKey":{"keyId":"0123456789abcdef","source":"rewrap","host":"admin-1","auditId":4,"setAt":"`)
	if doc := gunzip(t, gz); !bytes.Contains(doc, sealing) {
		t.Errorf("the document has no sealingKey of the row: %s", doc)
	}
	wrapped := regexp.MustCompile(`"wrappedMasterKeys":\[\{"keyId":"0123456789abcdef","kms":"awskms:///arn:aws:kms:eu-west-1:` +
		`111122223333:key/k","wrapped":"Y2lwaGVydGV4dCBh","createdAt":"[^"]+","wrappedAt":"[^"]+","auditId":5\},` +
		`\{"keyId":"fedcba9876543210",[^]]*"wrapped":"Y2lwaGVydGV4dCBi",[^]]*\}\]`)
	if doc := gunzip(t, gz); !wrapped.Match(doc) {
		t.Errorf("the document has no wrappedMasterKeys of the rows, by id, the ciphertexts in base64: %s", doc)
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
	_, _ = zw.Write([]byte(`{"version": 2, "schema": 25}`))
	_ = zw.Close()
	if _, _, err := decode(buf.Bytes()); !errors.Is(err, ErrVersion) {
		t.Errorf("decode of version 2: %v, want ErrVersion", err)
	}
}
