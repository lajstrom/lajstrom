package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/keys/keystest"
)

// lajstrom admin key wrap (docs/spec/index-schema.md, A master key in a KMS, #1388).

// wrapState reads the rows of wrapped_master_keys and the key.wrap rows of the SQLite file of dsn.
func wrapState(t *testing.T, dsn string) ([]index.WrappedMasterKey, []index.AuditEvent) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), dsn)
	must(t, err)
	defer func() { _ = st.Close() }()
	rows, err := st.ListWrappedMasterKeys(t.Context())
	must(t, err)
	evs, err := st.ListAudit(t.Context(), index.AuditFilter{Action: actionKeyWrap})
	must(t, err)
	return rows, evs
}

// checkWrapRow checks the key.wrap row of evs that row names, of a key made (made) or wrapped
// again, by dana for CHG-7; neither holds the ciphertext.
func checkWrapRow(t *testing.T, evs []index.AuditEvent, row index.WrappedMasterKey, made bool) {
	t.Helper()
	var ev index.AuditEvent
	for _, e := range evs {
		if row.AuditID != nil && e.ID == *row.AuditID {
			ev = e
		}
	}
	var d map[string]any
	if err := json.Unmarshal(ev.Details, &d); err != nil {
		t.Fatalf("no key.wrap row %v of the row %+v: %v", row.AuditID, row, err)
	}
	if ev.Subject != "master:"+row.KeyID || ev.Principal != "anonymous" || d["kms"] != row.KMS || d["new"] != made ||
		d["actor"] != "dana" || d["changeReason"] != "CHG-7" || d["source"] != "cli" || row.AuditID == nil || *row.AuditID != ev.ID {
		t.Errorf("the key.wrap row %+v, details %s, of the row %+v", ev, ev.Details, row)
	}
	if strings.Contains(string(ev.Details), base64.StdEncoding.EncodeToString(row.Wrapped)[:16]) {
		t.Errorf("the key.wrap row holds the ciphertext: %s", ev.Details)
	}
}

// TestAdminKeyWrap: admin key wrap --kms resolves an alias to its key's ARN, makes a data key,
// writes its row with key.wrap and prints the id and the entry of keys.masterKeyKMS, never the
// key or its ciphertext; with the entry, a command unwraps it. --id wraps it again under its KMS
// key, another ciphertext of the same key, key.wrap with new false. A key that cannot wrap, a GCP
// version, an id not configured, both flags and a KMS that refuses write nothing.
func TestAdminKeyWrap(t *testing.T) {
	kms := keystest.NewWrapKMS()
	cfg, dsn, env := kmsConfig(t, kms)
	who := []string{"--actor", "dana", "--reason", "CHG-7"}
	outs := runEnv(t, cfg, env, []step{
		{args: []string{"migrate"}},
		{args: append([]string{"admin", "key", "wrap", "--kms", "awskms:///" + kms.Alias}, who...)},
	})
	id, _, _ := strings.Cut(outs[1], "\n")
	entry := fmt.Sprintf("keys:\n  masterKeyKMS:\n    - {id: %s, kms: %q}\n", id, kms.URI())
	rows, evs := wrapState(t, dsn)
	if len(rows) != 1 || len(evs) != 1 || rows[0].KeyID != id || rows[0].KMS != kms.URI() || outs[1] != id+"\n"+entry {
		t.Fatalf("after a wrap: rows %+v, audit %+v, out %q", rows, evs, outs[1])
	}
	checkWrapRow(t, evs, rows[0], true)
	if strings.Contains(outs[1], base64.StdEncoding.EncodeToString(rows[0].Wrapped)[:16]) {
		t.Errorf("the output holds the ciphertext: %q", outs[1])
	}

	appendConfig(t, cfg, entry)
	outs = runEnv(t, cfg, env, []step{
		{args: []string{"admin", "key", "export", "--token"}},
		{args: append([]string{"admin", "key", "wrap", "--id", id}, who...), wantOut: id + "\n" + entry},
		{args: []string{"admin", "key", "export", "--token"}},
	})
	if outs[0] != outs[2] {
		t.Errorf("the token key after a wrap again: %q, want %q: the same master key opens it", outs[2], outs[0])
	}
	again, evs := wrapState(t, dsn)
	if len(again) != 1 || string(again[0].Wrapped) == string(rows[0].Wrapped) || !again[0].CreatedAt.Equal(rows[0].CreatedAt) ||
		again[0].WrappedAt.Before(rows[0].WrappedAt) || len(evs) != 2 {
		t.Fatalf("after a wrap again: %+v, %d key.wrap rows; want another ciphertext, the same creation", again, len(evs))
	}
	checkWrapRow(t, evs, again[0], false)

	kms.Disable(kms.ARN, "RSA_2048 ENCRYPT_DECRYPT Enabled")
	runEnv(t, cfg, env, []step{
		{args: []string{"admin", "key", "wrap", "--kms", kms.URI()}, wantErr: "is a key of RSA_2048 for ENCRYPT_DECRYPT"},
		{args: []string{"admin", "key", "wrap", "--id", id}, wantErr: "is a key of RSA_2048 for ENCRYPT_DECRYPT"},
		{args: []string{"admin", "key", "wrap", "--kms", "gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"},
			wantErr: "names a version of a key"},
		{args: []string{"admin", "key", "wrap", "--id", masterID(t, testMasterKey)}, wantErr: "is not a data key of keys.masterKeyKMS"},
		{args: []string{"admin", "key", "wrap", "--id", "0123"}, wantErr: "16 lowercase hexadecimal digits"},
		{args: []string{"admin", "key", "wrap", "--id", id, "--kms", kms.URI()}, wantErr: "none of the others can be"},
		{args: []string{"admin", "key", "wrap"}, wantErr: "at least one of the flags"},
	})
	kms.Disable(kms.ARN, "SYMMETRIC_DEFAULT ENCRYPT_DECRYPT Enabled")
	kms.Fail(fmt.Errorf("%w: down", keys.ErrKMSUnavailable))
	runEnv(t, cfg, env, []step{{args: []string{"admin", "key", "wrap", "--kms", kms.URI()}, wantErr: "ThrottlingException"}})
	if rows, evs := wrapState(t, dsn); len(rows) != 1 || len(evs) != 2 {
		t.Errorf("after the refused wraps: %d rows, %d key.wrap rows; want 1 and 2", len(rows), len(evs))
	}
}

// TestAdminKeyWrapReason: with auth.requireReason, admin key wrap without a reason is exit status
// 2 and writes nothing.
func TestAdminKeyWrapReason(t *testing.T) {
	kms := keystest.NewWrapKMS()
	cfg, dsn, env := kmsConfig(t, kms)
	appendConfig(t, cfg, "auth:\n  requireReason: true\n")
	runEnv(t, cfg, env, []step{{args: []string{"migrate"}}})
	_, _, err := run(t, env, "admin", "key", "wrap", "--kms", kms.URI(), "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "required (auth.requireReason)") || exitStatus(err) != 2 {
		t.Errorf("a wrap without a reason: %v (exit status %d)", err, exitStatus(err))
	}
	if rows, _ := wrapState(t, dsn); len(rows) != 0 {
		t.Errorf("a wrap without a reason wrote %d rows", len(rows))
	}
	runEnv(t, cfg, env, []step{{args: []string{"admin", "key", "wrap", "--kms", kms.URI(), "--reason", "CHG-8"}}})
}

// TestAdminKeyDropMaster: rewrap --dry-run lists the rows of wrapped_master_keys by id and KMS
// key, held or not, never a ciphertext; admin key drop --master refuses the key that seals and one
// that a live replica holds, and writes nothing; otherwise it deletes the row with key.drop,
// subject master:<id>, {kms}; a key without a row is refused.
func TestAdminKeyDropMaster(t *testing.T) {
	kms := keystest.NewWrapKMS()
	cfg, dsn, env := kmsConfig(t, kms)
	runEnv(t, cfg, env, []step{{args: []string{"migrate"}}})
	a, b, loose := plantDataKey(t, dsn, kms, kms.ARN, kms.URI()), plantDataKey(t, dsn, kms, kms.ARN, kms.URI()),
		plantDataKey(t, dsn, kms, kms.ARN, kms.URI())
	appendConfig(t, cfg, masterKeyKMS([]string{a.id, b.id}, []string{kms.URI(), kms.URI()}))
	st := openTestStore(t, dsn)
	ctx := t.Context()
	must(t, st.PutSealingKey(ctx, &index.SealingKey{KeyID: a.id, Source: index.SealingRewrap, Host: "admin"}, ""))
	outs := runEnv(t, cfg, env, []step{{args: []string{"admin", "key", "rewrap", "--dry-run"}}})
	var rep rewrapReport
	must(t, json.Unmarshal([]byte(outs[0]), &rep))
	held := map[string]bool{}
	for _, w := range rep.Wrapped {
		if w.KMS == kms.URI() {
			held[w.ID] = w.Held
		}
	}
	if fmt.Sprint(held) != fmt.Sprint(map[string]bool{a.id: true, b.id: true, loose.id: false}) {
		t.Errorf("the wrapped master keys of the dry run: %+v, want %s and %s held, %s not", rep.Wrapped, a.id, b.id, loose.id)
	}
	noSecretIn(t, errors.New(outs[0]), a, b, loose)

	must(t, st.StartReplica(ctx, &index.Replica{ID: "r1", Host: "node-1", PID: 7, MasterKeys: []string{a.id, b.id}},
		time.Now().Add(-time.Hour)))
	who := []string{"--actor", "dana", "--reason", "CHG-9"}
	runEnv(t, cfg, env, []step{
		{args: append([]string{"admin", "key", "drop", "--master", a.id}, who...), wantErr: "the index seals with master key " + a.id},
		{args: append([]string{"admin", "key", "drop", "--master", b.id}, who...), wantErr: "replica r1 on host node-1, process 7, " +
			"holds master key " + b.id},
		{args: []string{"admin", "key", "drop", "--master", "fedcba9876543210"}, wantErr: "has no row in wrapped_master_keys"},
		{args: []string{"admin", "key", "drop", "--master", "x"}, wantErr: "16 lowercase hexadecimal digits"},
		{args: []string{"admin", "key", "drop", "--master", b.id, "--token"}, wantErr: "none of the others can be"},
	})
	must(t, st.StopReplica(ctx, "r1"))
	runEnv(t, cfg, env, []step{
		{args: append([]string{"admin", "key", "drop", "--master", b.id}, who...), wantOut: b.id + "\n"},
		{args: []string{"admin", "key", "drop", "--master", b.id}, wantErr: "has no row in wrapped_master_keys"},
	})
	rows, err := st.ListWrappedMasterKeys(ctx)
	must(t, err)
	evs, err := st.ListAudit(ctx, index.AuditFilter{Action: keys.ActionDrop})
	must(t, err)
	if len(rows) != 2 || len(evs) != 1 || evs[0].Subject != "master:"+b.id || !strings.Contains(string(evs[0].Details),
		`"kms":"`+kms.URI()+`"`) || !strings.Contains(string(evs[0].Details), `"changeReason":"CHG-9"`) {
		t.Errorf("after the drop: %d rows, key.drop rows %+v; want 2 rows and one key.drop of %s", len(rows), evs, b.id)
	}
}

// TestAdminKeyDropMasterReason: with auth.requireReason, admin key drop --master without a reason
// is exit status 2 and deletes nothing.
func TestAdminKeyDropMasterReason(t *testing.T) {
	kms := keystest.NewWrapKMS()
	cfg, dsn, env := kmsConfig(t, kms)
	appendConfig(t, cfg, "auth:\n  requireReason: true\n")
	runEnv(t, cfg, env, []step{{args: []string{"migrate"}}})
	k := plantDataKey(t, dsn, kms, kms.ARN, kms.URI())
	_, _, err := run(t, env, "admin", "key", "drop", "--master", k.id, "--config", cfg)
	if err == nil || !strings.Contains(err.Error(), "required (auth.requireReason)") || exitStatus(err) != 2 {
		t.Errorf("a drop without a reason: %v (exit status %d)", err, exitStatus(err))
	}
	if rows, _ := wrapState(t, dsn); len(rows) != 1 {
		t.Errorf("a drop without a reason left %d rows, want 1", len(rows))
	}
	runEnv(t, cfg, env, []step{{args: []string{"admin", "key", "drop", "--master", k.id, "--reason", "CHG-10"}, wantOut: k.id}})
}
