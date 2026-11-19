package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// The sealing key at the start (docs/spec/index-schema.md, The master key and its replacement,
// #1288).

// otherMasterKey and thirdMasterKey are master keys of the tests besides testMasterKey.
var (
	otherMasterKey = strings.Repeat("fedcba9876543210", 4)
	thirdMasterKey = strings.Repeat("00112233445566778899aabbccddeeff", 2)
)

// testMasters returns the master keys of c and env over st, with the key the index names to seal
// (holdMasters), or fails the test.
func testMasters(t *testing.T, c *config.Config, env func(string) (string, bool), st sealingStore) masters {
	t.Helper()
	a := &app{cfg: c, lookupEnv: env}
	ms, err := a.holdMasters(t.Context(), st, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// masterID returns the id of the master key hex.
func masterID(t *testing.T, hex string) string {
	t.Helper()
	id, err := keys.MasterID(hex)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// heldOf returns the Master of the keys hexes, none sealing.
func heldOf(t *testing.T, hexes ...string) *keys.Master {
	t.Helper()
	var srcs []keys.MasterSource
	for i, h := range hexes {
		srcs = append(srcs, keys.MasterSource{Source: "key" + string(rune('0'+i)), Text: h})
	}
	m, err := keys.HoldMasters(srcs)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// switchSealing makes to the sealing key of the SQLite file of dsn where it is from, as the switch
// of a rewrap does.
func switchSealing(t *testing.T, dsn, from, to string) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.PutSealingKey(t.Context(), &index.SealingKey{KeyID: to, Source: index.SealingRewrap, Host: "admin"}, from); err != nil {
		t.Fatal(err)
	}
}

// newSealingStore returns a new migrated SQLite file.
func newSealingStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(t.Context(), "file:"+filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return st
}

// noKeyIn fails the test when err shows a key of the tests.
func noKeyIn(t *testing.T, err error) {
	t.Helper()
	for _, k := range []string{testMasterKey, otherMasterKey, thirdMasterKey} {
		if err != nil && strings.Contains(err.Error(), k[:16]) {
			t.Errorf("an error shows a key: %v", err)
		}
	}
}

// TestSealingAtStart: without a row, a process of one key records it, source start, and seals
// with it; with the row, a process that holds its key seals with it whatever else it holds, and
// one that does not stops, naming the id and the ids it holds, never a key.
func TestSealingAtStart(t *testing.T) {
	ctx := t.Context()
	a, b := masterID(t, testMasterKey), masterID(t, otherMasterKey)
	st := newSealingStore(t)
	m := heldOf(t, testMasterKey)
	if err := sealingAtStart(ctx, st, m, "host-1"); err != nil {
		t.Fatal(err)
	}
	row, err := st.GetSealingKey(ctx)
	if err != nil || row.KeyID != a || row.Source != index.SealingStart || row.Host != "host-1" || row.AuditID != nil {
		t.Fatalf("the row of the first start: %+v, %v; want %s, start, host-1", row, err, a)
	}
	if id, held := m.Sealing(); id != a || !held {
		t.Errorf("Sealing = %s, %t; want %s, true", id, held, a)
	}
	both := heldOf(t, otherMasterKey, testMasterKey)
	if err := sealingAtStart(ctx, st, both, "host-2"); err != nil {
		t.Fatalf("a process of both keys on a row of one: %v", err)
	}
	if id, _ := both.Sealing(); id != a {
		t.Errorf("a process of both keys seals with %s, want the row's %s", id, a)
	}
	for _, err := range []error{
		sealingAtStart(ctx, st, heldOf(t, otherMasterKey), "host-3"),
		sealingAtStart(ctx, st, heldOf(t, otherMasterKey, thirdMasterKey), "host-3"),
	} {
		want := "the index seals with master key " + a + ", which this process does not hold; it holds "
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), b) {
			t.Errorf("a process without the row's key: %v, want %q and %s", err, want, b)
		}
		noKeyIn(t, err)
	}
}

// TestSealingAtStartSeveral: without a row, a process of several keys records nothing and stops,
// naming the rewrap and the ids it holds: which seals is the operator's choice.
func TestSealingAtStartSeveral(t *testing.T) {
	ctx := t.Context()
	st := newSealingStore(t)
	err := sealingAtStart(ctx, st, heldOf(t, testMasterKey, otherMasterKey), "host-1")
	if err == nil || !strings.Contains(err.Error(), "no master key seals yet: name one with lajstrom admin key rewrap --to <id>") ||
		!strings.Contains(err.Error(), masterID(t, testMasterKey)) || !strings.Contains(err.Error(), masterID(t, otherMasterKey)) {
		t.Errorf("several keys without a row: %v", err)
	}
	noKeyIn(t, err)
	if _, err := st.GetSealingKey(ctx); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("several keys without a row recorded one: %v", err)
	}
}

// TestSealingAtStartFirstKey: without a row, a process whose one key does not open the first key
// of the index records nothing and stops, naming that row; the key that opens it records itself.
func TestSealingAtStartFirstKey(t *testing.T) {
	ctx := t.Context()
	st := newSealingStore(t)
	under := heldOf(t, otherMasterKey)
	under.SealWith(masterID(t, otherMasterKey))
	sealed, err := under.Seal("FP1", []byte("a private key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateKey(ctx, &index.Key{Kind: "jwt", Public: []byte("p"), PrivateEnc: sealed, Fingerprint: "FP1"}); err != nil {
		t.Fatal(err)
	}
	err = sealingAtStart(ctx, st, heldOf(t, testMasterKey), "host-1")
	if err == nil || !strings.Contains(err.Error(), "does not open the first key of the index (keys id 1, fingerprint FP1)") {
		t.Errorf("a key that does not open the first key: %v", err)
	}
	noKeyIn(t, err)
	if _, err := st.GetSealingKey(ctx); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a key that does not open the first key recorded itself: %v", err)
	}
	if err := sealingAtStart(ctx, st, heldOf(t, otherMasterKey), "host-1"); err != nil {
		t.Errorf("the key that opens the first key: %v", err)
	}
}

// TestSealingAtStartRace: processes that start together, each with its one key, record one row;
// those of the other keys stop, naming it.
func TestSealingAtStartRace(t *testing.T) {
	st := newSealingStore(t)
	hexes := []string{testMasterKey, otherMasterKey, thirdMasterKey, testMasterKey}
	errs := make([]error, len(hexes))
	var wg sync.WaitGroup
	for i, h := range hexes {
		wg.Go(func() { errs[i] = sealingAtStart(context.Background(), st, heldOf(t, h), "host") })
	}
	wg.Wait()
	row, err := st.GetSealingKey(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i, h := range hexes {
		if holds := masterID(t, h) == row.KeyID; holds != (errs[i] == nil) {
			t.Errorf("process %d (key %s, the row's %s): %v", i, masterID(t, h), row.KeyID, errs[i])
		}
	}
}

// TestServeSealingAtStart: serve records its one key at its first start, logs the ids of its keys
// with their sources and the one that seals, and never a key, and writes its ids in its replicas
// row; on a database whose sealing key it does not hold it stops, naming the id, and a command
// stops the same way.
func TestServeSealingAtStart(t *testing.T) {
	dir := t.TempDir()
	cfg, addr := serveConfig(t, dir)
	appendConfig(t, cfg, "keys:\n  masterKeyEnv: LAJSTROM_MASTER_KEY\n")
	env := map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}
	var logged syncBuffer
	stop := startServeEnv(t, cfg, addr, env, &logged, "--log-format", "json")
	st, err := sqlite.Open(t.Context(), "file:"+filepath.Join(dir, "lajstrom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	a := masterID(t, testMasterKey)
	rows, err := st.ListReplicas(t.Context())
	if err != nil || len(rows) != 1 || len(rows[0].MasterKeys) != 1 || rows[0].MasterKeys[0] != a {
		t.Errorf("the replicas row: %+v, %v; want the id %s", rows, err, a)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), `"msg":"master keys","keys":{"`+a+`":"LAJSTROM_MASTER_KEY"},"sealing":"`+a+`"`) ||
		strings.Contains(logged.String(), testMasterKey[:16]) {
		t.Errorf("the start's log:\n%s", logged.String())
	}
	b := masterID(t, otherMasterKey)
	switchSealing(t, "file:"+filepath.Join(dir, "lajstrom.db"), a, b)
	want := "the index seals with master key " + b + ", which this process does not hold; it holds " + a
	runEnv(t, cfg, env, []step{
		{args: []string{"serve"}, wantErr: want},
		{args: []string{"admin", "key", "export", "--token"}, wantErr: want},
	})
}

// TestPreferSealing: a restore neither stops on the sealing row nor inserts one; it tries the key
// the row names first when it holds it, and leaves the order as it is otherwise.
func TestPreferSealing(t *testing.T) {
	ctx := t.Context()
	st := newSealingStore(t)
	m := heldOf(t, testMasterKey, otherMasterKey)
	if err := errors.Join(preferSealing(ctx, st, m), preferSealing(ctx, st, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSealingKey(ctx); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a restore without a row inserted one: %v", err)
	}
	c := masterID(t, thirdMasterKey)
	if err := st.PutSealingKey(ctx, &index.SealingKey{KeyID: c, Source: index.SealingStart, Host: "h"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := preferSealing(ctx, st, m); err != nil {
		t.Errorf("a restore on a row of a key it does not hold: %v", err)
	}
	if id, _ := m.Sealing(); id != "" {
		t.Errorf("a restore on a row of a key it does not hold made %s the sealing key", id)
	}
	b := masterID(t, otherMasterKey)
	if err := st.PutSealingKey(ctx, &index.SealingKey{KeyID: b, Source: index.SealingRewrap, Host: "h"}, c); err != nil {
		t.Fatal(err)
	}
	if err := preferSealing(ctx, st, m); err != nil {
		t.Fatal(err)
	}
	if id, held := m.Sealing(); id != b || !held {
		t.Errorf("Sealing = %s, %t after a restore on a row of a key it holds; want %s, true", id, held, b)
	}
}

// TestReplicaSealing: at each beat a replica reads the sealing key again. A switch to another key
// it holds seals with that key from then on, logged at info, and the replica stays ready. A switch
// to a key it does not hold fails closed from that beat on: it seals nothing, the key it sealed
// with before included, and is not ready, naming the id, while it opens all the while; every beat
// logs an error naming the id. A switch back to a key it holds ends it. No line shows a key.
func TestReplicaSealing(t *testing.T) {
	ctx := t.Context()
	st := openIndex(t, t.TempDir())
	a, b, c := masterID(t, testMasterKey), masterID(t, otherMasterKey), masterID(t, thirdMasterKey)
	if err := st.PutSealingKey(ctx, &index.SealingKey{KeyID: a, Source: index.SealingStart, Host: "h"}, ""); err != nil {
		t.Fatal(err)
	}
	m := heldOf(t, testMasterKey, otherMasterKey)
	if err := sealingAtStart(ctx, st, m, "h"); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	r, err := startReplica(ctx, st, slog.New(slog.NewTextHandler(&logs, nil)), time.Now, "", m)
	if err != nil {
		t.Fatal(err)
	}
	if row := oneReplica(t, st); len(row.MasterKeys) != 2 {
		t.Errorf("the replicas row holds %v, want the two ids", row.MasterKeys)
	}
	sealedA, err := m.Seal("aad", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	for i, tt := range []struct {
		to       string
		level    string // of the line of the beat, "" for none
		readyErr string
	}{
		{a, "", ""},
		{b, "INFO", ""},
		{c, "ERROR", "this replica does not hold the master key that seals: " + c},
		{c, "ERROR", "this replica does not hold the master key that seals: " + c},
		{a, "INFO", ""},
	} {
		if row, err := st.GetSealingKey(ctx); err != nil || row.KeyID != tt.to {
			if err := st.PutSealingKey(ctx, &index.SealingKey{KeyID: tt.to, Source: index.SealingRewrap, Host: "h"}, row.KeyID); err != nil {
				t.Fatal(err)
			}
		}
		logs.Reset()
		r.sealing(ctx)
		if level := logLevel(logs.String()); level != tt.level || (tt.level != "" && !strings.Contains(logs.String(), tt.to)) {
			t.Errorf("beat %d to %s: logged %q, want a line of level %q naming the id:\n%s", i, tt.to, level, tt.level, logs.String())
		}
		checkBeat(t, fmt.Sprintf("beat %d to %s", i, tt.to), st, r, sealedA, tt.readyErr)
		noKeyIn(t, errors.New(logs.String()))
	}
}

// checkBeat checks the replica r after a beat: readiness answers readyErr, or nil for "", and its
// keys seal while it is ready and seal nothing otherwise, and open sealed, a value under a key it
// holds, either way.
func checkBeat(t *testing.T, what string, st store, r *replica, sealed []byte, readyErr string) {
	t.Helper()
	switch err := ready(slog.New(slog.DiscardHandler), "sqlite", st, newRegistryHealth(nil, slog.New(slog.DiscardHandler)), r)(t.Context()); {
	case readyErr == "" && err != nil, readyErr != "" && (err == nil || err.Error() != readyErr):
		t.Errorf("%s: readiness = %v, want %q", what, err, readyErr)
	}
	m := r.master
	if _, err := m.Seal("aad", []byte("secret")); (err == nil) != (readyErr == "") {
		t.Errorf("%s: Seal = %v, want a seal: %t", what, err, readyErr == "")
	}
	if got, err := m.Open("aad", sealed); err != nil || string(got) != "secret" {
		t.Errorf("%s: Open = %q, %v; want the secret, whatever seals", what, got, err)
	}
}
