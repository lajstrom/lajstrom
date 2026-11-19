package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/keys/keystest"
)

// serve and the data keys of keys.masterKeyKMS (docs/spec/index-schema.md, A master key in a KMS,
// #1388): serve listens without waiting for the KMS.

// lockedBuffer is a log of several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// kmsApp is the app of serve over a configuration of the data keys ks, wrapped by kms's first
// key, with kms as their client and logs as its log.
func kmsApp(kms *keystest.WrapKMS, logs io.Writer, ks ...plantedKey) *app {
	c := &config.Config{}
	for _, k := range ks {
		c.Keys.MasterKeyKMS = append(c.Keys.MasterKeyKMS, config.MasterKeyKMS{ID: k.id, KMS: kms.URI()})
	}
	return &app{cfg: c, lookupEnv: noEnv, log: slog.New(slog.NewTextHandler(logs, nil)),
		wrapKMS: map[string]keys.MasterKMS{config.KMSSchemeAWS: kms}}
}

// quickUnwraps makes the waits between the unwraps of serve a millisecond for the test.
func quickUnwraps(t *testing.T) {
	t.Helper()
	before := unwrapBackoff
	unwrapBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { unwrapBackoff = before })
}

// awaitHeld waits until m holds id, at most 10 s.
func awaitHeld(t *testing.T, m *keys.Master, id string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !m.Holds(id); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s is not held 10 s after the KMS answers again", id)
		}
	}
}

// TestUnwrapWait: after a start that did not unwrap, serve asks again after 1, 2, 4, 8 and 16 s,
// then every 30 s.
func TestUnwrapWait(t *testing.T) {
	var got []time.Duration
	for n := range 8 {
		got = append(got, unwrapWait(n))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second,
		30 * time.Second, 30 * time.Second}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the waits = %v, want %v", got, want)
	}
}

// TestServeUnwrapLater is K-6 of the threat model: a serve whose sealing key is a data key that
// the KMS does not unwrap at its start starts all the same, seals nothing and is not ready, names
// the id, logs the failure at error, and lists no such key in its replicas row; asked again once
// the KMS answers, the key joins the held keys and is never asked for again, and the next beat
// writes it in the row and seals with it, ready.
func TestServeUnwrapLater(t *testing.T) {
	quickUnwraps(t)
	ctx := t.Context()
	dir := t.TempDir()
	st := openIndex(t, dir)
	kms := keystest.NewWrapKMS()
	k := plantDataKey(t, "file:"+dir+"/lajstrom.db", kms, kms.ARN, kms.URI())
	must(t, st.PutSealingKey(ctx, &index.SealingKey{KeyID: k.id, Source: index.SealingRewrap, Host: "admin"}, ""))
	var logs lockedBuffer
	a := kmsApp(kms, &logs, k)
	kms.Fail(fmt.Errorf("%w: down", keys.ErrKMSUnavailable))
	ms, rep, pending, err := a.serveKeys(ctx, st, "")
	must(t, err)
	t.Cleanup(func() { rep.Stop(ctx) })
	want := "this replica has not unwrapped the master key that seals: " + k.id
	if err := rep.Err(); err == nil || err.Error() != want || len(pending) != 1 {
		t.Fatalf("readiness = %v, %d keys asked again; want %q and the key", err, len(pending), want)
	}
	if !strings.Contains(logs.String(), "level=ERROR msg=\"master key not unwrapped yet") || !strings.Contains(logs.String(), k.id) {
		t.Errorf("the start's log has no error that names the id:\n%s", logs.String())
	}
	if row := oneReplica(t, st); len(row.MasterKeys) != 0 {
		t.Errorf("the replicas row lists %v before the unwrap, want none", row.MasterKeys)
	}
	var se *keys.SealingError
	if _, err := ms.m.Seal("aad", []byte("x")); !errors.As(err, &se) || !se.Pending {
		t.Errorf("a seal before the unwrap: %v, want a SealingError of a pending key", err)
	}

	kms.Fail(nil)
	unwrapLater(ctx, a.log, ms.m, pending)
	awaitHeld(t, ms.m, k.id)
	n := kms.Decrypts()
	logs.reset()
	rep.beat(ctx)
	if row := oneReplica(t, st); len(row.MasterKeys) != 1 || row.MasterKeys[0] != k.id {
		t.Errorf("the replicas row after the beat: %v, want %s", row.MasterKeys, k.id)
	}
	if err := rep.Err(); err != nil {
		t.Errorf("readiness after the beat: %v", err)
	}
	if _, err := ms.m.Seal("aad", []byte("x")); err != nil {
		t.Errorf("a seal after the beat: %v", err)
	}
	if !strings.Contains(logs.String(), "the master key that seals is unwrapped") {
		t.Errorf("the beat's log:\n%s", logs.String())
	}
	rep.beat(ctx)
	if kms.Decrypts() != n {
		t.Errorf("the KMS was asked %d times more after the unwrap, want none", kms.Decrypts()-n)
	}
	noSecretIn(t, errors.New(logs.String()), k)
}

// TestServeUnwrapFirstKey: on a database that names no key, a serve whose sole key is a data key
// that does not open at its start records none and is not ready; the beat after the unwrap records
// it, source start, and seals with it.
func TestServeUnwrapFirstKey(t *testing.T) {
	quickUnwraps(t)
	ctx := t.Context()
	dir := t.TempDir()
	st := openIndex(t, dir)
	kms := keystest.NewWrapKMS()
	k := plantDataKey(t, "file:"+dir+"/lajstrom.db", kms, kms.ARN, kms.URI())
	a := kmsApp(kms, io.Discard, k)
	kms.Fail(errors.New("AccessDeniedException: not yet"))
	ms, rep, pending, err := a.serveKeys(ctx, st, "")
	must(t, err)
	t.Cleanup(func() { rep.Stop(ctx) })
	if _, err := st.GetSealingKey(ctx); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the sealing key before the unwrap: %v, want none recorded", err)
	}
	if err := rep.Err(); err == nil || !strings.Contains(err.Error(), "has not unwrapped the master key that seals: "+k.id) {
		t.Errorf("readiness before the unwrap: %v", err)
	}
	kms.Fail(nil)
	unwrapLater(ctx, a.log, ms.m, pending)
	awaitHeld(t, ms.m, k.id)
	rep.beat(ctx)
	if row, err := st.GetSealingKey(ctx); err != nil || row.KeyID != k.id || row.Source != index.SealingStart {
		t.Errorf("the sealing key after the beat: %+v, %v; want %s of a start", row, err, k.id)
	}
	if err := rep.Err(); err != nil {
		t.Errorf("readiness after the beat: %v", err)
	}
}

// TestServeUnwrapOtherID is K-5 at the start of serve: a row whose ciphertext the KMS unwraps to
// another key is never held, is logged at error, and is not asked for again.
func TestServeUnwrapOtherID(t *testing.T) {
	quickUnwraps(t)
	ctx := t.Context()
	dir := t.TempDir()
	st := openIndex(t, dir)
	kms := keystest.NewWrapKMS()
	k := plantDataKey(t, "file:"+dir+"/lajstrom.db", kms, kms.ARN, kms.URI())
	other := make([]byte, 32)
	planted, err := kms.Encrypt(ctx, kms.ARN, other, keys.MasterAD(k.id))
	must(t, err)
	must(t, st.PutWrappedMasterKey(ctx, &index.WrappedMasterKey{KeyID: k.id, KMS: kms.URI(), Wrapped: planted},
		mustWrapped(t, st, k.id)))
	var logs lockedBuffer
	a := kmsApp(kms, &logs, k)
	ms, rep, pending, err := a.serveKeys(ctx, st, "")
	must(t, err)
	t.Cleanup(func() { rep.Stop(ctx) })
	n := kms.Decrypts()
	unwrapLater(ctx, a.log, ms.m, pending)
	time.Sleep(20 * time.Millisecond)
	if len(pending) != 0 || ms.m.Holds(k.id) || kms.Decrypts() != n {
		t.Errorf("%d keys asked again, held %t, %d unwraps after the start; want none", len(pending), ms.m.Holds(k.id), kms.Decrypts()-n)
	}
	if !strings.Contains(logs.String(), "level=ERROR msg=\"master key not held: its KMS key unwrapped another key") {
		t.Errorf("the start's log has no error of another key:\n%s", logs.String())
	}
	if err := rep.Err(); err == nil {
		t.Error("ready with no key that seals")
	}
}

// mustWrapped returns the ciphertext of the row of id.
func mustWrapped(t *testing.T, st index.Reader, id string) []byte {
	t.Helper()
	rows, err := st.ListWrappedMasterKeys(t.Context())
	must(t, err)
	for _, r := range rows {
		if r.KeyID == id {
			return r.Wrapped
		}
	}
	t.Fatalf("no row of %s", id)
	return nil
}

// TestServeKMSDown: serve, through AWS KMS that does not answer, listens all the same:
// /-/healthz answers 200 and /-/readyz 503, naming the id of the data key that seals.
func TestServeKMSDown(t *testing.T) {
	kms := keystest.NewWrapKMS()
	cfg, dsn, env := kmsConfig(t, kms)
	cfg, addr := serveOn(t, cfg)
	runEnv(t, cfg, env, []step{{args: []string{"migrate"}}})
	k := plantDataKey(t, dsn, kms, kms.ARN, kms.URI())
	appendConfig(t, cfg, masterKeyKMS([]string{k.id}, []string{kms.URI()}))
	runEnv(t, cfg, env, []step{{args: []string{"admin", "key", "export", "--token"}}}) // the sealing key, and a token key under it
	kms.Fail(fmt.Errorf("%w: down", keys.ErrKMSUnavailable))
	startServeEnv(t, cfg, addr, env, io.Discard)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/-/readyz", nil)
	must(t, err)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable ||
		!strings.Contains(string(body), "this replica has not unwrapped the master key that seals: "+k.id) {
		t.Errorf("/-/readyz = %d %s, want 503 naming %s", resp.StatusCode, body, k.id)
	}
}

// serveOn returns cfg listening on a free address of loopback, without metrics, and that address.
func serveOn(t *testing.T, cfg string) (string, string) {
	t.Helper()
	_, addr := serveConfig(t, t.TempDir())
	b, err := os.ReadFile(cfg) // #nosec G304 -- the test's own file
	must(t, err)
	body := strings.Replace(string(b), `listen: "127.0.0.1:0"`, `listen: "`+addr+`"`+"\n  metricsListen: \"\"\n  shutdownDelay: 0s", 1)
	must(t, os.WriteFile(cfg, []byte(body), 0o600)) //nolint:gosec // the test's own file
	return cfg, addr
}
