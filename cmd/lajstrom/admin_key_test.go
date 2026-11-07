package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
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
		{args: []string{"admin", "key", "export"}, wantErr: `required flag(s) "repo" not set`},
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
