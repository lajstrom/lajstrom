package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// sealedUnder returns plain sealed for aad under the one key hex.
func sealedUnder(t *testing.T, hex, aad, plain string) []byte {
	t.Helper()
	m := heldOf(t, hex)
	m.SealWith(masterID(t, hex))
	b, err := m.Seal(aad, []byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rewrapFixture is a database whose sealed values are under three keys: the old one (A,
// testMasterKey), the new one (B, otherMasterKey) and a lost one (C, thirdMasterKey).
type rewrapFixture struct {
	st                             store
	a, b, c                        string // the ids
	keyIDs, webhookIDs, refreshIDs []int64
	plain                          map[string]string // aad to its plaintext
}

// newRewrapFixture fills a SQLite file: an apt repo's active and retired keys and the server's
// bearer-token key under A, a policy key under B, and a repo's key under C; two webhooks under A,
// one under B; an OIDC login's refresh token under A, one under B (#1282); an open upload of an
// encrypted repo under A, one under C, one of a repo that does not encrypt, and a committed one
// under A.
func newRewrapFixture(t *testing.T) *rewrapFixture {
	t.Helper()
	return fillRewrapFixture(t, newSealingStore(t))
}

// fillRewrapFixture fills st as newRewrapFixture does.
func fillRewrapFixture(t *testing.T, st store) *rewrapFixture {
	t.Helper()
	ctx := t.Context()
	f := &rewrapFixture{st: st, a: masterID(t, testMasterKey), b: masterID(t, otherMasterKey),
		c: masterID(t, thirdMasterKey), plain: map[string]string{}}
	repo := index.Repo{Format: "apt", Name: "debs", Registry: "local", Prefix: "apt/debs", Kind: index.KindHosted}
	if err := f.st.CreateRepo(ctx, &repo); err != nil {
		t.Fatal(err)
	}
	retired := time.Now().Add(-time.Hour)
	for i, k := range []struct {
		repo    *int64
		kind    string
		hex     string
		retired *time.Time
	}{
		{&repo.ID, "gpg", testMasterKey, &retired},
		{&repo.ID, "gpg", testMasterKey, nil},
		{nil, "jwt", testMasterKey, nil},
		{nil, "policy", otherMasterKey, nil},
		{&repo.ID, "enc", thirdMasterKey, nil},
	} {
		fp := fmt.Sprintf("FP%d", i)
		f.plain[fp] = "private key " + fp
		row := index.Key{RepoID: k.repo, Kind: k.kind, Public: []byte("pub"), PrivateEnc: sealedUnder(t, k.hex, fp, f.plain[fp]),
			Fingerprint: fp, RetiredAt: k.retired}
		if err := f.st.CreateKey(ctx, &row); err != nil {
			t.Fatal(err)
		}
		f.keyIDs = append(f.keyIDs, row.ID)
	}
	for i, hex := range []string{testMasterKey, testMasterKey, otherMasterKey} {
		h := index.Webhook{UID: index.NewUID(time.Now()), Mask: "apt/*", URL: fmt.Sprintf("https://hooks.example.com/%d", i),
			Active: true, CreatedBy: "user:alice"}
		f.plain["webhook:"+h.UID] = "webhook secret " + h.URL
		h.SecretEnc = sealedUnder(t, hex, "webhook:"+h.UID, f.plain["webhook:"+h.UID])
		if err := f.st.CreateWebhook(ctx, &h); err != nil {
			t.Fatal(err)
		}
		f.webhookIDs = append(f.webhookIDs, h.ID)
	}
	f.refreshIDs = fillRefreshTokens(t, st, f.plain, testMasterKey, otherMasterKey)
	for i, u := range []struct {
		hex, state string
		expires    time.Duration
	}{{testMasterKey, "open", 30 * time.Minute}, {testMasterKey, "open", 50 * time.Minute}, {thirdMasterKey, "open", 40 * time.Minute},
		{"", "open", time.Hour}, {testMasterKey, "committed", time.Hour}} {
		key := fmt.Sprintf("blob-upload:u%d", i)
		files := map[string]any{"name": "apt/debs/x"}
		if u.hex != "" {
			files["enc"] = map[string]any{"key": sealedUnder(t, u.hex, key, "data key"), "nonce": []byte("n")}
		}
		raw, _ := json.Marshal(files)
		s := index.Session{RepoID: repo.ID, Key: key, Principal: "user:alice", State: u.state, Files: raw,
			ExpiresAt: time.Now().Add(u.expires).UTC().Truncate(time.Second)}
		if err := f.st.UpsertSession(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// fillRefreshTokens adds to st an OIDC login's refresh token sealed under each key of hexes, its
// plaintext in plain by its additional data, and returns the ids of their rows of oidc_refresh.
func fillRefreshTokens(t *testing.T, st store, plain map[string]string, hexes ...string) []int64 {
	t.Helper()
	ctx := t.Context()
	u := index.User{Name: fmt.Sprintf("oidc-%d", len(plain)), Kind: "oidc"}
	if err := st.CreateUser(ctx, &u); err != nil {
		t.Fatal(err)
	}
	ident := index.Identity{UserID: u.ID, Issuer: "https://idp.example", Subject: u.Name}
	if err := st.CreateIdentity(ctx, &ident); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i, hex := range hexes {
		row := index.OIDCRefresh{LoginID: ptrTo(int64(1000 + i)), IdentityID: ident.ID, Sealed: []byte{0}, NextAt: time.Now()}
		if err := st.CreateOIDCRefresh(ctx, &row); err != nil {
			t.Fatal(err)
		}
		aad := auth.RefreshAAD(row.ID)
		plain[aad] = fmt.Sprintf("rt-refresh-token-%d", i)
		if ok, err := st.Reseal(ctx, index.SealedRefresh, row.ID, []byte{0}, sealedUnder(t, hex, aad, plain[aad])); err != nil || !ok {
			t.Fatalf("the refresh token %d: %v, %v", i, ok, err)
		}
		ids = append(ids, row.ID)
	}
	return ids
}

// TestCountSealed: the values of keys, webhooks and refresh tokens are counted under the key that
// opens each,
// retired keys included; those under no key held are named by their rows, a key with its kind and
// fingerprint; the open uploads that hold a data key are counted under theirs, with the time the
// last of them expires, those under no key held under "none", and neither an upload that does
// not encrypt nor one that is not open counts.
func TestCountSealed(t *testing.T) {
	f := newRewrapFixture(t)
	m := heldOf(t, testMasterKey, otherMasterKey)
	c, err := countSealed(t.Context(), f.st, m)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]keyCounts{f.a: {Keys: 3, Webhooks: 2, Refresh: 1}, f.b: {Keys: 1, Webhooks: 1, Refresh: 1}}
	if fmt.Sprint(c.Under) != fmt.Sprint(want) {
		t.Errorf("Under = %v, want %v", c.Under, want)
	}
	if len(c.Lost) != 1 || c.Lost[0] != (lostValue{Table: index.SealedKeys, ID: f.keyIDs[4], Kind: "enc", Fingerprint: "FP4"}) ||
		c.Lost[0].String() != fmt.Sprintf("keys id %d (enc FP4)", f.keyIDs[4]) {
		t.Errorf("Lost = %+v", c.Lost)
	}
	if s := c.Sessions[f.a]; s.Open != 2 || time.Until(s.LastExpiresAt) < 45*time.Minute || len(c.Sessions) != 2 ||
		c.Sessions[noKey].Open != 1 {
		t.Errorf("Sessions = %+v; want 2 under %s, the last expiring in 50 minutes, and 1 under none", c.Sessions, f.a)
	}
	if other := c.other(f.b); len(other) != 1 || other[f.a].Keys != 3 {
		t.Errorf("other(%s) = %v", f.b, other)
	}
}

// withReplicas is a store whose replicas are rows: SQLite holds one live row at most.
type withReplicas struct {
	store
	rows []index.Replica
}

func (s withReplicas) ListReplicas(context.Context) ([]index.Replica, error) { return s.rows, nil }

// TestRewrapGuard: a rewrap refuses a key the command does not hold, and one that a live replica
// does not list among its keys, one that lists none (a release before 0040) included; a stale row
// does not count. Each refusal names the replica by its host, pid and version, and the key by its
// id.
func TestRewrapGuard(t *testing.T) {
	ctx := t.Context()
	st := newSealingStore(t)
	a, b, c := masterID(t, testMasterKey), masterID(t, otherMasterKey), masterID(t, thirdMasterKey)
	m := heldOf(t, testMasterKey, otherMasterKey)
	now, err := st.DatabaseTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	both := index.Replica{ID: "both", Host: "pod-1", PID: 1, Version: "v1.1.0", HeartbeatAt: now.Add(-time.Minute), MasterKeys: []string{a, b}}
	stale := index.Replica{ID: "stale", Host: "pod-0", PID: 9, Version: "v1.0.0", HeartbeatAt: now.Add(-3 * time.Minute)}
	old := index.Replica{ID: "old", Host: "pod-2", PID: 2, Version: "v1.0.0", HeartbeatAt: now}
	if refusals, err := rewrapGuard(ctx, withReplicas{st, []index.Replica{both, stale}}, m, b); err != nil || len(refusals) != 0 {
		t.Errorf("guard with every live replica holding the key: %q, %v", refusals, err)
	}
	refusals, err := rewrapGuard(ctx, withReplicas{st, []index.Replica{both, stale}}, m, c)
	if err != nil || len(refusals) != 2 || !strings.Contains(refusals[0], "this command does not hold master key "+c) ||
		!strings.Contains(refusals[1], "the replica on pod-1 (pid 1, v1.1.0) does not hold master key "+c+"; it holds "+a+", "+b) {
		t.Errorf("guard of a key nobody holds: %q, %v", refusals, err)
	}
	refusals, err = rewrapGuard(ctx, withReplicas{st, []index.Replica{both, old}}, m, b)
	if err != nil || len(refusals) != 1 || !strings.Contains(refusals[0], "the replica on pod-2 (pid 2, v1.0.0) does not hold master key "+
		b+"; it lists no master key: a release before 0040") {
		t.Errorf("guard with a replica of the release before: %q, %v", refusals, err)
	}
}

// TestReseal: every value under another key held is sealed again under the key the command seals
// with, retired keys and webhooks included, and opens to the same plaintext; a value under that key
// already is left byte for byte, and so is one under no key held; the uploads are not sealed
// again. A second run seals nothing.
func TestReseal(t *testing.T) {
	ctx := t.Context()
	f := newRewrapFixture(t)
	before := sealedValues(t, f.st)
	m := heldOf(t, testMasterKey, otherMasterKey)
	m.SealWith(f.b)
	done, err := reseal(ctx, f.st, m, f.b)
	if err != nil || done != (keyCounts{Keys: 3, Webhooks: 2, Refresh: 1}) {
		t.Fatalf("reseal = %+v, %v; want 3 keys, 2 webhooks and a refresh token", done, err)
	}
	after := sealedValues(t, f.st)
	for _, id := range []string{fmt.Sprint("keys ", f.keyIDs[3]), fmt.Sprint("keys ", f.keyIDs[4]), fmt.Sprint("webhooks ", f.webhookIDs[2]),
		fmt.Sprint("oidc_refresh ", f.refreshIDs[1])} {
		if !bytes.Equal(before[id].Value, after[id].Value) {
			t.Errorf("%s changed: it was under the new key, or under none", id)
		}
	}
	onlyB := heldOf(t, otherMasterKey)
	for id, v := range after {
		if id == fmt.Sprint("keys ", f.keyIDs[4]) {
			continue
		}
		plain, err := onlyB.Open(v.AAD, v.Value)
		if err != nil || string(plain) != f.plain[v.AAD] {
			t.Errorf("%s under the new key alone: %q, %v", id, plain, err)
		}
	}
	c, err := countSealed(ctx, f.st, m)
	if err != nil || len(c.other(f.b)) != 0 || c.Sessions[f.a].Open != 2 {
		t.Errorf("after the rewrap: %+v, %v; want every value under %s and the uploads as they were", c, err, f.b)
	}
	if done, err := reseal(ctx, f.st, m, f.b); err != nil || done != (keyCounts{}) {
		t.Errorf("a second run = %+v, %v; want nothing sealed again", done, err)
	}
}

// sealedValues returns the sealed values of keys and webhooks of st, by table and id.
func sealedValues(t *testing.T, st store) map[string]index.Sealed {
	t.Helper()
	out := map[string]index.Sealed{}
	for _, table := range sealedTables {
		page, err := st.ListSealed(t.Context(), table, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range page {
			out[fmt.Sprint(table, " ", v.ID)] = v
		}
	}
	return out
}

// changing is a store whose first ListSealed of webhooks writes, after its read, a new secret of
// the first webhook under the old key: a rotation between the read of a batch and its write.
type changing struct {
	store
	hex  string
	once sync.Once
}

func (s *changing) ListSealed(ctx context.Context, table string, afterID int64, limit int) ([]index.Sealed, error) {
	page, err := s.store.ListSealed(ctx, table, afterID, limit)
	if table == index.SealedWebhooks && len(page) > 0 && err == nil {
		s.once.Do(func() {
			h, gerr := s.GetWebhook(ctx, page[0].ID)
			if gerr != nil {
				err = gerr
				return
			}
			h.SecretEnc, err = sealOne(s.hex, page[0].AAD, "a rotated secret")
			if err == nil {
				err = s.UpdateWebhook(ctx, h)
			}
		})
	}
	return page, err
}

// sealOne seals plain for aad under the one key hex.
func sealOne(hex, aad, plain string) ([]byte, error) {
	m, err := keys.ParseMaster(hex)
	if err != nil {
		return nil, err
	}
	return m.Seal(aad, []byte(plain))
}

// TestResealChanged: a row whose value changed between the read of its batch and the write is
// left by that batch, read again after the last one, and sealed again then: the rotated secret
// is under the new key at the end, never the value the batch read.
func TestResealChanged(t *testing.T) {
	ctx := t.Context()
	f := newRewrapFixture(t)
	m := heldOf(t, testMasterKey, otherMasterKey)
	m.SealWith(f.b)
	st := &changing{store: f.st, hex: testMasterKey}
	done, err := reseal(ctx, st, m, f.b)
	if err != nil || done != (keyCounts{Keys: 3, Webhooks: 2, Refresh: 1}) {
		t.Fatalf("reseal = %+v, %v; want 3 keys, 2 webhooks and a refresh token, the rotated one among them", done, err)
	}
	h, err := f.st.GetWebhook(ctx, f.webhookIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	plain, by, err := m.OpenKey("webhook:"+h.UID, h.SecretEnc)
	if err != nil || by != f.b || string(plain) != "a rotated secret" {
		t.Errorf("the rotated secret: %q under %s, %v; want the rotated secret under %s", plain, by, err, f.b)
	}
}

// TestResealTwice: two runs at once seal each value once: the counts of both add up to the values
// under the old key.
func TestResealTwice(t *testing.T) {
	f := newRewrapFixture(t)
	var wg sync.WaitGroup
	var dones [2]keyCounts
	var errs [2]error
	for i := range 2 {
		wg.Go(func() {
			m := heldOf(t, testMasterKey, otherMasterKey)
			m.SealWith(f.b)
			dones[i], errs[i] = reseal(context.Background(), f.st, m, f.b)
		})
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || dones[0].Keys+dones[1].Keys != 3 || dones[0].Webhooks+dones[1].Webhooks != 2 ||
		dones[0].Refresh+dones[1].Refresh != 1 {
		t.Errorf("two runs: %+v, %v; want 3 keys, 2 webhooks and a refresh token in all", dones, errs)
	}
}
