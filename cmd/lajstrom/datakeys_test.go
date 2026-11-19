package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/keys/keystest"
)

// The data keys of keys.masterKeyKMS at the start of a command (docs/spec/index-schema.md, A
// master key in a KMS, #1388).

// plantedKey is a data key of the tests: its 32 bytes in hex, its id, and the ciphertext of its
// row.
type plantedKey struct{ hex, id, wrapped string }

// plantDataKey makes a data key, has kms wrap it under its id with the key arn, and writes its row
// into the SQLite file of dsn under the key URI uri, as lajstrom admin key wrap would.
func plantDataKey(t *testing.T, dsn string, kms *keystest.WrapKMS, arn, uri string) plantedKey {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	k := plantedKey{hex: hex.EncodeToString(b)}
	k.id = masterID(t, k.hex)
	wrapped, err := kms.Encrypt(t.Context(), arn, b, keys.MasterAD(k.id))
	if err != nil {
		t.Fatal(err)
	}
	k.wrapped = base64.StdEncoding.EncodeToString(wrapped)
	st, err := sqlite.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.PutWrappedMasterKey(t.Context(), &index.WrappedMasterKey{KeyID: k.id, KMS: uri, Wrapped: wrapped}, nil); err != nil {
		t.Fatal(err)
	}
	return k
}

// kmsConfig writes a configuration of keys.masterKeyKMS alone, the entries of ids under the key
// URI uri, and returns it with the DSN of its database and the environment of AWS KMS at the
// endpoint of kms.
func kmsConfig(t *testing.T, kms *keystest.WrapKMS) (cfg, dsn string, env map[string]string) {
	t.Helper()
	awsTestEnv(t)
	srv := httptest.NewServer(keystest.AWSWrapKMS(kms))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg, dsn = filepath.Join(dir, "lajstrom.yaml"), "file:"+filepath.Join(dir, "lajstrom.db")
	body := "server:\n  listen: \"127.0.0.1:0\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"" + dsn +
		"\"\nregistries:\n  - name: local\n    url: \"http://localhost:5000\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, dsn, map[string]string{"AWS_ENDPOINT_URL_KMS": srv.URL}
}

// masterKeyKMS is the keys section of entries of ids, each under the key URI uris[i].
func masterKeyKMS(ids, uris []string) string {
	s := "keys:\n  masterKeyKMS:\n"
	for i, id := range ids {
		s += fmt.Sprintf("    - {id: %s, kms: %q}\n", id, uris[i])
	}
	return s
}

// noSecretIn fails the test when err shows a key of ks, or its ciphertext.
func noSecretIn(t *testing.T, err error, ks ...plantedKey) {
	t.Helper()
	for _, k := range ks {
		if err != nil && (strings.Contains(err.Error(), k.hex[:16]) || strings.Contains(err.Error(), k.wrapped[:16])) {
			t.Errorf("an error shows a key or a ciphertext: %v", err)
		}
	}
}

// TestMasterKeyKMSRow is K-5 of the threat model (#1388): a command whose configuration names a
// data key unwraps it at its start through AWS KMS, and seals with it, the sole key, on a new
// database. An entry without a row, or whose row is under another KMS key than the entry's,
// stops serve and every command before they write, naming the id and the KMS keys: the database
// never chooses which key a process holds, nor which KMS key it calls.
func TestMasterKeyKMSRow(t *testing.T) {
	kms := keystest.NewWrapKMS()
	other := "arn:aws:kms:eu-west-1:111122223333:key/other"
	kms.AddKey(other)
	cfg, dsn, env := kmsConfig(t, kms)
	runEnv(t, cfg, env, []step{{args: []string{"migrate"}}})
	good := plantDataKey(t, dsn, kms, kms.ARN, kms.URI())
	moved := plantDataKey(t, dsn, kms, other, "awskms:///"+other)
	missing := masterID(t, otherMasterKey)
	base, err := os.ReadFile(cfg) // #nosec G304 -- the test's own file
	must(t, err)
	write := func(ids, uris []string) {
		t.Helper()
		must(t, os.WriteFile(cfg, append(append([]byte{}, base...), masterKeyKMS(ids, uris)...), 0o600)) //nolint:gosec // the test's own file
	}

	write([]string{good.id}, []string{kms.URI()})
	runEnv(t, cfg, env, []step{{args: []string{"admin", "key", "export", "--token"}, wantOut: "PUBLIC KEY"}})
	checkSealing(t, dsn, good.id)

	for _, tt := range []struct {
		ids, uris []string
		want      string
	}{
		{[]string{good.id, missing}, []string{kms.URI(), kms.URI()}, "master key " + missing + " has no row in wrapped_master_keys"},
		{[]string{moved.id}, []string{kms.URI()}, "master key " + moved.id + " is wrapped by awskms:///" + other +
			" in this database, not by " + kms.URI()},
	} {
		write(tt.ids, tt.uris)
		n := kms.Decrypts()
		for _, args := range [][]string{{"admin", "key", "rotate", "--token"}, {"serve"}} {
			_, _, err := run(t, env, append(args, "--config", cfg)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%v with %v: %v, want an error with %q", args, tt.ids, err, tt.want)
			}
			noSecretIn(t, err, good, moved)
		}
		if kms.Decrypts() != n && len(tt.ids) == 1 {
			t.Errorf("%v: the KMS was asked to unwrap a row the configuration does not name", tt.ids)
		}
	}
	checkTokenKeys1(t, dsn)
}

// TestCommandUnwrap: a command whose data key does not open, the KMS refusing or not answering,
// stops before it writes, naming the id, the KMS key and which of the two it was, never a key or
// a ciphertext.
func TestCommandUnwrap(t *testing.T) {
	kms := keystest.NewWrapKMS()
	cfg, dsn, env := kmsConfig(t, kms)
	runEnv(t, cfg, env, []step{{args: []string{"migrate"}}})
	k := plantDataKey(t, dsn, kms, kms.ARN, kms.URI())
	appendConfig(t, cfg, masterKeyKMS([]string{k.id}, []string{kms.URI()}))
	for _, tt := range []struct {
		fail error
		want string
	}{
		{fmt.Errorf("%w: down", keys.ErrKMSUnavailable), "master key " + k.id + ": the KMS key " + kms.URI() + " did not answer"},
		{errors.New("AccessDeniedException: not this role"), "master key " + k.id + ": the KMS key " + kms.URI() + " refused to unwrap it"},
	} {
		kms.Fail(tt.fail)
		for _, args := range [][]string{{"admin", "key", "export", "--token"}, {"admin", "key", "rewrap", "--dry-run"}, {"reindex"}} {
			_, _, err := run(t, env, append(args, "--config", cfg)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%v: %v, want an error with %q", args, err, tt.want)
			}
			noSecretIn(t, err, k)
		}
	}
	kms.Fail(nil)
	st, err := sqlite.Open(t.Context(), dsn)
	must(t, err)
	defer func() { _ = st.Close() }()
	if _, err := st.GetSealingKey(t.Context()); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the sealing key after commands that did not unwrap: %v, want none written", err)
	}
}

// checkSealing checks that the sealing row of the SQLite file of dsn names id.
func checkSealing(t *testing.T, dsn, id string) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), dsn)
	must(t, err)
	defer func() { _ = st.Close() }()
	if row, err := st.GetSealingKey(t.Context()); err != nil || row.KeyID != id {
		t.Errorf("the sealing key: %+v, %v; want %s", row, err, id)
	}
}

// checkTokenKeys1 checks that the SQLite file of dsn holds one token key: no command refused
// above made or rotated one.
func checkTokenKeys1(t *testing.T, dsn string) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), dsn)
	must(t, err)
	defer func() { _ = st.Close() }()
	if ks, err := st.ListKeys(t.Context(), nil); err != nil || len(ks) != 1 {
		t.Errorf("the token keys: %d, %v; want the one of the first export", len(ks), err)
	}
}
