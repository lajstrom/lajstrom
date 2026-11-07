package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// testMasterKey is the master key of the tests, built at run time so that no scanner takes a
// literal for a credential.
var testMasterKey = strings.Repeat("0123456789abcdef", 4)

// writeKeyConfig writes a configuration with a SQLite file in dir, the registry local and
// keys.masterKeyEnv LAJSTROM_MASTER_KEY, and returns its path and the database's DSN.
func writeKeyConfig(t *testing.T, dir string) (cfg, dsn string) {
	t.Helper()
	cfg, dsn = filepath.Join(dir, "lajstrom.yaml"), "file:"+filepath.Join(dir, "lajstrom.db")
	body := "server:\n  listen: \":8080\"\ndatabase:\n  driver: sqlite\n  dsn: \"" + dsn + "\"\n" +
		"registries:\n  - name: local\n    url: \"http://localhost:5000\"\nkeys:\n  masterKeyEnv: LAJSTROM_MASTER_KEY\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, dsn
}

// runEnv runs steps with --config cfg appended and env as the environment.
func runEnv(t *testing.T, cfg string, env map[string]string, steps []step) []string {
	t.Helper()
	var outs []string
	for _, s := range steps {
		out, _, err := run(t, env, append(s.args, "--config", cfg)...)
		switch {
		case s.wantErr == "" && err != nil:
			t.Fatalf("%v: %v", s.args, err)
		case s.wantErr != "" && (err == nil || !strings.Contains(err.Error(), s.wantErr)):
			t.Fatalf("%v: error = %v, want it to contain %q", s.args, err, s.wantErr)
		}
		if !strings.Contains(out, s.wantOut) {
			t.Errorf("%v: stdout = %q, want it to contain %q", s.args, out, s.wantOut)
		}
		if err != nil && strings.Contains(err.Error(), testMasterKey[:16]) {
			t.Errorf("%v: the error shows the master key: %v", s.args, err)
		}
		outs = append(outs, out)
	}
	return outs
}

// fingerprintsOf returns the fingerprints of an armored keyring.
func fingerprintsOf(t *testing.T, armored string) []string {
	t.Helper()
	el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil {
		t.Fatalf("keyring %q: %v", armored, err)
	}
	var fps []string
	for _, e := range el {
		fps = append(fps, strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint)))
	}
	return fps
}

// TestAdminKey runs admin key export and rotate against a migrated SQLite file with the master
// key in the environment: export makes the key and prints the keyring, a second export prints
// the same, rotate prints the new fingerprint, and export then prints the new key and the
// retired one; the rotation has its audit row. The private key in the database is ciphertext.
func TestAdminKey(t *testing.T) {
	cfg, dsn := writeKeyConfig(t, t.TempDir())
	env := map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}
	runEnv(t, cfg, env, []step{
		{args: []string{"migrate"}, wantOut: "applied 0004"},
		{args: []string{"admin", "repo", "create", "apt/ubuntu", "--registry", "local"}},
	})
	outs := runEnv(t, cfg, env, []step{
		{args: []string{"admin", "key", "export", "--repo", "apt/ubuntu"}, wantOut: "-----BEGIN PGP PUBLIC KEY BLOCK-----\n"},
		{args: []string{"admin", "key", "export", "--repo", "apt/ubuntu"}},
		{args: []string{"admin", "key", "rotate", "--repo", "apt/ubuntu"}},
		{args: []string{"admin", "key", "export", "--repo", "apt/ubuntu"}},
	})
	first := fingerprintsOf(t, outs[0])
	if len(first) != 1 || outs[1] != outs[0] {
		t.Fatalf("export made %v; the second export is the same: %t", first, outs[1] == outs[0])
	}
	rotated := strings.TrimSuffix(outs[2], "\n")
	if !regexp.MustCompile(`^[0-9A-F]{40}$`).MatchString(rotated) || rotated == first[0] {
		t.Fatalf("rotate printed %q, the first key is %s", outs[2], first[0])
	}
	if got := fingerprintsOf(t, outs[3]); len(got) != 2 || got[0] != rotated || got[1] != first[0] {
		t.Errorf("export after the rotation: %v, want %s and the retired %s", got, rotated, first[0])
	}
	checkRotated(t, dsn, rotated)
}

// checkRotated checks the database at dsn after one rotation of apt/ubuntu to the key of
// fingerprint: the audit row, two keys, and no private key in the clear.
func checkRotated(t *testing.T, dsn, fingerprint string) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	evs, err := st.ListAudit(context.Background(), index.AuditFilter{Action: "key.rotate"})
	if err != nil || len(evs) != 1 || evs[0].Repo != "apt/ubuntu" || evs[0].Subject != fingerprint {
		t.Errorf("key.rotate rows %+v, %v", evs, err)
	}
	repo, err := st.GetRepo(context.Background(), "apt", "ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListKeys(context.Background(), &repo.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("keys %+v, %v", rows, err)
	}
	for _, k := range rows {
		if el, err := openpgp.ReadKeyRing(bytes.NewReader(k.PrivateEnc)); err == nil && len(el) > 0 && el[0].PrivateKey != nil {
			t.Errorf("the private key of %s is stored in the clear", k.Fingerprint)
		}
	}
}

// TestAdminKeyErrors checks the errors of admin key: without the master key both commands fail
// and say which variable and what it must hold; a repo that is not apt, one that does not
// exist, and a missing --repo.
func TestAdminKeyErrors(t *testing.T) {
	cfg, _ := writeKeyConfig(t, t.TempDir())
	env := map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}
	runEnv(t, cfg, env, []step{
		{args: []string{"migrate"}},
		{args: []string{"admin", "repo", "create", "apt/ubuntu", "--registry", "local"}},
		{args: []string{"admin", "repo", "create", "raw/scratch", "--registry", "local"}},
	})
	for _, cmd := range []string{"export", "rotate"} {
		args := []string{"admin", "key", cmd, "--repo", "apt/ubuntu"}
		runEnv(t, cfg, nil, []step{{args: args, wantErr: "admin key needs the master key: keys: no master key: keys.masterKeyEnv names LAJSTROM_MASTER_KEY, which is not set"}})
		runEnv(t, cfg, map[string]string{"LAJSTROM_MASTER_KEY": ""}, []step{{args: args,
			wantErr: "the environment variable LAJSTROM_MASTER_KEY is empty; the master key is 64 hexadecimal characters (32 bytes), e.g. the output of openssl rand -hex 32"}})
		runEnv(t, cfg, map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey[:40]}, []step{{args: args,
			wantErr: "keys.masterKeyEnv: the environment variable LAJSTROM_MASTER_KEY: not a master key: the master key is 64 hexadecimal characters"}})
	}
	plain, _ := writeRepoConfig(t, t.TempDir())
	runEnv(t, plain, env, []step{{args: []string{"admin", "key", "export", "--repo", "apt/ubuntu"}, wantErr: "keys.masterKeyEnv is not set"}})
	runEnv(t, cfg, env, []step{
		{args: []string{"admin", "key", "export", "--repo", "raw/scratch"}, wantErr: `repo "raw/scratch": only apt repos have signing keys`},
		{args: []string{"admin", "key", "rotate", "--repo", "apt/nope"}, wantErr: "repo apt/nope does not exist"},
		{args: []string{"admin", "key", "export", "--repo", "ubuntu"}, wantErr: "want <format>/<name>"},
		{args: []string{"admin", "key", "export"}, wantErr: "at least one of the flags in the group [repo token] is required"},
	})
}

// TestServeMasterKey checks that serve stops before it listens when the master key is
// configured but unusable: the variable empty or malformed, or unset while the index holds
// signing keys. The error names the variable, never a value.
func TestServeMasterKey(t *testing.T) {
	cfg, _ := writeKeyConfig(t, t.TempDir())
	env := map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}
	runEnv(t, cfg, env, []step{
		{args: []string{"migrate"}},
		{args: []string{"admin", "repo", "create", "apt/ubuntu", "--registry", "local"}},
	})
	runEnv(t, cfg, map[string]string{"LAJSTROM_MASTER_KEY": ""}, []step{{args: []string{"serve"}, wantErr: "LAJSTROM_MASTER_KEY is empty"}})
	runEnv(t, cfg, map[string]string{"LAJSTROM_MASTER_KEY": "xyz"}, []step{{args: []string{"serve"}, wantErr: "64 hexadecimal characters (32 bytes)"}})
	runEnv(t, cfg, env, []step{{args: []string{"admin", "key", "export", "--repo", "apt/ubuntu"}}})
	runEnv(t, cfg, nil, []step{{args: []string{"serve"},
		wantErr: "keys.masterKeyEnv names LAJSTROM_MASTER_KEY, which is not set, and the index holds 1 signing keys that only the master key decrypts"}})
}

// TestNewSigner checks the Signer of serve: the keys service with the master key; nil with a
// warning per apt repo when the variable is not set and the index holds no key, or when
// keys.masterKeyEnv is empty.
func TestNewSigner(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:"+filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range []index.Repo{{Format: "apt", Name: "ubuntu"}, {Format: "apt", Name: "debian"}, {Format: "raw", Name: "scratch"}} {
		r.Registry, r.Kind = "local", "hosted"
		if err := st.CreateRepo(ctx, &r); err != nil {
			t.Fatal(err)
		}
	}
	c := config.Default()
	c.Keys.MasterKeyEnv = "LAJSTROM_MASTER_KEY"
	for _, tt := range []struct {
		name     string
		envName  string
		env      map[string]string
		signs    bool
		warnings int
	}{
		{"the master key", "LAJSTROM_MASTER_KEY", map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}, true, 0},
		{"the variable unset", "LAJSTROM_MASTER_KEY", nil, false, 2},
		{"keys.masterKeyEnv empty", "", map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}, false, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logged bytes.Buffer
			c.Keys.MasterKeyEnv = tt.envName
			signer, err := newSigner(ctx, c, func(k string) (string, bool) { v, ok := tt.env[k]; return v, ok }, st,
				slog.New(slog.NewTextHandler(&logged, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if (signer != nil) != tt.signs {
				t.Errorf("signer = %v, want one: %t", signer, tt.signs)
			}
			if n := strings.Count(logged.String(), "apt repo is unsigned: no master key"); n != tt.warnings {
				t.Errorf("%d warnings, want %d:\n%s", n, tt.warnings, logged.String())
			}
			if strings.Contains(logged.String(), testMasterKey[:16]) {
				t.Errorf("the log shows the master key:\n%s", logged.String())
			}
		})
	}
}

// pemFingerprint returns the fingerprint of the public key of a PEM block "PUBLIC KEY": the
// hexadecimal sha256 of its PKIX DER.
func pemFingerprint(t *testing.T, s string) string {
	t.Helper()
	b, rest := pem.Decode([]byte(s))
	if b == nil || b.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("not one PEM public key: %q", s)
	}
	if _, err := x509.ParsePKIXPublicKey(b.Bytes); err != nil {
		t.Fatalf("the PEM block is not a PKIX public key: %v", err)
	}
	sum := sha256.Sum256(b.Bytes)
	return hex.EncodeToString(sum[:])
}

// TestAdminKeyToken runs admin key for the bearer-token key against a migrated SQLite file with
// the master key: export makes the key and prints its public key as PEM, again the same;
// rotate --token prints a new fingerprint, rotate --token --drop another, drop --token retires
// the active key without a successor and a second drop has nothing to drop. The database then
// holds three jwt rows without a repo: the first retired now (it verifies for the grace), the
// other two one grace ago (dropped), the audit rows of each change, and no private key in the
// clear.
func TestAdminKeyToken(t *testing.T) {
	cfg, dsn := writeKeyConfig(t, t.TempDir())
	env := map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}
	start := time.Now()
	outs := runEnv(t, cfg, env, []step{
		{args: []string{"migrate"}},
		{args: []string{"admin", "key", "export", "--token"}, wantOut: "-----BEGIN PUBLIC KEY-----\n"},
		{args: []string{"admin", "key", "export", "--token"}},
		{args: []string{"admin", "key", "rotate", "--token"}},
		{args: []string{"admin", "key", "rotate", "--token", "--drop"}},
		{args: []string{"admin", "key", "drop", "--token"}},
		{args: []string{"admin", "key", "drop", "--token"}, wantErr: "keys: there is no active token key"},
	})
	end := time.Now()
	first := pemFingerprint(t, outs[1])
	second, third := strings.TrimSpace(outs[3]), strings.TrimSpace(outs[4])
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if outs[2] != outs[1] || !hex64.MatchString(second) || !hex64.MatchString(third) || second == first || third == second {
		t.Fatalf("export %s twice the same: %t; rotate printed %q and %q", first, outs[2] == outs[1], outs[3], outs[4])
	}
	if dropped := strings.TrimSpace(outs[5]); dropped != third {
		t.Errorf("drop printed %q, want the active key %s", dropped, third)
	}
	checkTokenKeys(t, dsn, start, end, first, second, third)
}

// checkTokenKeys checks the database at dsn after TestAdminKeyToken, whose commands ran between
// start and end: the three jwt rows, the first retired by the rotation, the others dropped, the
// audit rows, and no private key in the clear.
func checkTokenKeys(t *testing.T, dsn string, start, end time.Time, first, second, third string) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	rows, err := st.ListKeys(t.Context(), nil)
	if err != nil || len(rows) != 3 {
		t.Fatalf("server keys %v, %v; want 3", rows, err)
	}
	for i, fp := range []string{first, second, third} {
		k := rows[i]
		switch {
		case k.Kind != keys.KindJWT || k.RepoID != nil || k.Fingerprint != fp || k.RetiredAt == nil:
			t.Errorf("row %d: %s %v %s retired %v, want jwt, no repo, %s, retired", i, k.Kind, k.RepoID, k.Fingerprint, k.RetiredAt, fp)
		case i == 0 && k.RetiredAt.Before(start.Add(-time.Second)):
			t.Errorf("the rotated key was retired at %v, before the test started", k.RetiredAt)
		case i > 0 && k.RetiredAt.After(end.Add(-keys.TokenGrace)):
			t.Errorf("the dropped key %s was retired at %v, within the grace", fp, k.RetiredAt)
		}
		if _, err := x509.ParsePKCS8PrivateKey(k.PrivateEnc); err == nil {
			t.Errorf("the private key of %s is stored in the clear", fp)
		}
	}
	checkAudit(t, st,
		"key.rotate  "+second+` {"retired":"`+first+`"}`,
		"key.rotate  "+third+` {"dropped":true,"retired":"`+second+`"}`,
		"key.drop  "+third+" ")
}

// checkAudit checks the audit rows of st, each "<action> <repo> <subject> <details>", in any
// order.
func checkAudit(t *testing.T, st index.Reader, want ...string) {
	t.Helper()
	evs, err := st.ListAudit(t.Context(), index.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range evs {
		got = append(got, ev.Action+" "+ev.Repo+" "+ev.Subject+" "+string(ev.Details))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("audit rows\n%q\nwant\n%q", got, want)
	}
}

// TestAdminKeyTokenErrors checks the flags of the token commands, one of --repo and --token and
// not both, --drop with --token only, drop needs --token, and the commands without the master
// key, whose error says what serve does then.
func TestAdminKeyTokenErrors(t *testing.T) {
	cfg, _ := writeKeyConfig(t, t.TempDir())
	env := map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}
	runEnv(t, cfg, env, []step{
		{args: []string{"migrate"}},
		{args: []string{"admin", "key", "rotate"}, wantErr: "at least one of the flags in the group [repo token] is required"},
		{args: []string{"admin", "key", "rotate", "--repo", "apt/ubuntu", "--token"},
			wantErr: "if any flags in the group [repo token] are set none of the others can be"},
		{args: []string{"admin", "key", "export", "--repo", "apt/ubuntu", "--token"},
			wantErr: "if any flags in the group [repo token] are set none of the others can be"},
		{args: []string{"admin", "key", "rotate", "--repo", "apt/ubuntu", "--drop"}, wantErr: "--drop goes with --token"},
		{args: []string{"admin", "key", "drop"}, wantErr: `required flag(s) "token" not set`},
	})
	for _, args := range [][]string{
		{"admin", "key", "rotate", "--token"}, {"admin", "key", "export", "--token"}, {"admin", "key", "drop", "--token"},
	} {
		runEnv(t, cfg, nil, []step{{args: args, wantErr: "admin key needs the master key: keys: no master key: keys.masterKeyEnv names " +
			"LAJSTROM_MASTER_KEY, which is not set; without it bearer tokens are signed with an ephemeral key: they do not " +
			"survive a restart and are not shared between replicas; set keys.masterKeyEnv"}})
		runEnv(t, cfg, map[string]string{"LAJSTROM_MASTER_KEY": "xyz"}, []step{{args: args, wantErr: "64 hexadecimal characters"}})
	}
}

// TestServeTokenKey checks that serve stops before it listens when the active token key does
// not decrypt (the database made under another master key), naming the key and admin key drop
// --token; after the drop, newTokenKeys makes a new key under the new master key. A file that
// still sets auth.tokenSigningKeyFile stops serve at Load.
func TestServeTokenKey(t *testing.T) {
	cfg, dsn := writeKeyConfig(t, t.TempDir())
	other := strings.Repeat("fedcba9876543210", 4)
	outs := runEnv(t, cfg, map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}, []step{
		{args: []string{"migrate"}},
		{args: []string{"admin", "key", "export", "--token"}},
	})
	fp := pemFingerprint(t, outs[1])
	env := map[string]string{"LAJSTROM_MASTER_KEY": other}
	runEnv(t, cfg, env, []step{{args: []string{"serve"}, wantErr: "the bearer-token key: keys: the private key of " + fp +
		" does not decrypt with the master key: another master key, or the row was changed; start with the master key " +
		"that made it, or retire it with lajstrom admin key drop --token, and a new key is made"}})
	runEnv(t, cfg, env, []step{{args: []string{"admin", "key", "drop", "--token"}, wantOut: fp}})

	st, err := sqlite.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	c := config.Default()
	c.Keys.MasterKeyEnv = "LAJSTROM_MASTER_KEY"
	tokens, err := newTokenKeys(t.Context(), c, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, st,
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if k, err := tokens.TokenSigner(t.Context()); err != nil || k.ID() == fp {
		t.Errorf("after the drop: %v, %v; want a new key", k, err)
	}

	gone := filepath.Join(t.TempDir(), "gone.yaml")
	if err := os.WriteFile(gone, []byte("server:\n  listen: \":8080\"\nauth:\n  tokenSigningKeyFile: /etc/lajstrom/jwt.key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runEnv(t, gone, env, []step{{args: []string{"serve"}, wantErr: "auth.tokenSigningKeyFile is gone: the key lives in the database, ADR 0010"}})
}

// TestNewTokenKeys checks the token keys of serve: with the master key, the key of the index,
// made at the first start and the same at the next, logged at info with its fingerprint; without
// it, an ephemeral key and the warning; a variable that is set but malformed is an error. No
// line shows the master key.
func TestNewTokenKeys(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:"+filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.Keys.MasterKeyEnv = "LAJSTROM_MASTER_KEY"
	lookup := func(env map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	}
	withKey := lookup(map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey})
	var fps []string
	for range 2 {
		var logged bytes.Buffer
		tokens, err := newTokenKeys(ctx, c, withKey, st, slog.New(slog.NewTextHandler(&logged, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := tokens.(*keys.Tokens); !ok {
			t.Fatalf("with the master key: %T, want *keys.Tokens", tokens)
		}
		k, err := tokens.TokenSigner(ctx)
		if err != nil {
			t.Fatal(err)
		}
		fps = append(fps, k.ID())
		if !strings.Contains(logged.String(), `msg="bearer tokens are signed with the key in the database" fingerprint=`+k.ID()+" algorithm=ES256") ||
			strings.Contains(logged.String(), testMasterKey[:16]) {
			t.Errorf("log:\n%s", logged.String())
		}
	}
	if fps[0] != fps[1] {
		t.Errorf("the second start signs with %s, the first made %s", fps[1], fps[0])
	}

	var logged bytes.Buffer
	tokens, err := newTokenKeys(ctx, c, lookup(nil), st, slog.New(slog.NewTextHandler(&logged, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tokens.(*keys.EphemeralTokens); !ok {
		t.Errorf("without the master key: %T, want *keys.EphemeralTokens", tokens)
	}
	if !strings.Contains(logged.String(), "level=WARN msg=\"bearer tokens are signed with an ephemeral key: they do not survive a restart "+
		"and are not shared between replicas; set keys.masterKeyEnv\"") {
		t.Errorf("no warning:\n%s", logged.String())
	}
	if _, err := newTokenKeys(ctx, c, lookup(map[string]string{"LAJSTROM_MASTER_KEY": "xyz"}), st, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("a malformed master key: no error")
	}
}

// TestWarnOpen checks the startup line of serve without an Authorizer, and its absence with one.
func TestWarnOpen(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	warnOpen(log, nil)
	if want := `level=WARN msg="authorization is not enforced until RBAC (#45): every principal, anonymous included, may do ` +
		`everything on /v2 and the frontends, and /v2/token therefore grants every requested scope"`; !strings.Contains(logged.String(), want) {
		t.Errorf("log %q, want %q", logged.String(), want)
	}
	logged.Reset()
	warnOpen(log, denyAll{})
	if logged.Len() != 0 {
		t.Errorf("with an Authorizer: %q", logged.String())
	}
}

// denyAll is an Authorizer that allows nothing.
type denyAll struct{}

func (denyAll) Allowed(context.Context, auth.Principal, auth.Action, string, string) (bool, error) {
	return false, nil
}
