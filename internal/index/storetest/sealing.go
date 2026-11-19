package storetest

import (
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The master keys (#1288; docs/spec/index-schema.md, The master key and its replacement): the
// sealed values a page at a time and written again by compare-and-set, and the sealing key.

// testSealed: ListSealed answers the values of keys and of webhooks above an id, by id, at most a
// limit, retired keys included, each with its additional data; Reseal writes a value only where
// the row still holds the one read, changes nothing else of the row, and answers false for a row
// changed or deleted since; another table is an error.
func testSealed(t *testing.T, s index.Store) {
	ctx := t.Context()
	r := newRepo(t, s, "apt", "signed")
	var keys []index.Key
	for i, k := range []index.Key{
		{RepoID: &r.ID, Kind: "gpg", Public: []byte("p1"), PrivateEnc: []byte("sealed 1"), Fingerprint: "FP1", RetiredAt: ptr(at(10))},
		{RepoID: &r.ID, Kind: "gpg", Public: []byte("p2"), PrivateEnc: []byte("sealed 2"), Fingerprint: "FP2"},
		{Kind: "jwt", Public: []byte("p3"), PrivateEnc: []byte("sealed 3"), Fingerprint: "FP3"},
	} {
		k.CreatedAt = at(i)
		must(t, s.CreateKey(ctx, &k))
		keys = append(keys, k)
	}
	hook := newWebhook(t, s, "apt/*")

	page, err := s.ListSealed(ctx, index.SealedKeys, 0, 2)
	must(t, err)
	same(t, "the first page of keys", []index.Sealed{
		{Table: index.SealedKeys, ID: keys[0].ID, AAD: "FP1", Value: []byte("sealed 1")},
		{Table: index.SealedKeys, ID: keys[1].ID, AAD: "FP2", Value: []byte("sealed 2")},
	}, page)
	page, err = s.ListSealed(ctx, index.SealedKeys, keys[1].ID, 2)
	must(t, err)
	same(t, "the second page of keys", []index.Sealed{{Table: index.SealedKeys, ID: keys[2].ID, AAD: "FP3", Value: []byte("sealed 3")}}, page)
	page, err = s.ListSealed(ctx, index.SealedKeys, keys[2].ID, 2)
	must(t, err)
	same(t, "past the last key", 0, len(page))
	page, err = s.ListSealed(ctx, index.SealedWebhooks, 0, 100)
	must(t, err)
	same(t, "the webhooks", []index.Sealed{{Table: index.SealedWebhooks, ID: hook.ID, AAD: "webhook:" + hook.UID,
		Value: hook.SecretEnc}}, page)
	if _, err := s.ListSealed(ctx, "users", 0, 10); err == nil {
		t.Error("ListSealed of users succeeded")
	}

	ok, err := s.Reseal(ctx, index.SealedKeys, keys[0].ID, []byte("sealed 1"), []byte("again 1"))
	must(t, err)
	if !ok {
		t.Error("Reseal of the value read: false, want true")
	}
	ok, err = s.Reseal(ctx, index.SealedKeys, keys[1].ID, []byte("not what it holds"), []byte("again 2"))
	must(t, err)
	if ok {
		t.Error("Reseal over a value changed since: true, want false")
	}
	ok, err = s.Reseal(ctx, index.SealedKeys, keys[2].ID+100, []byte("sealed 3"), []byte("again 3"))
	must(t, err)
	if ok {
		t.Error("Reseal of a row that does not exist: true, want false")
	}
	ok, err = s.Reseal(ctx, index.SealedWebhooks, hook.ID, hook.SecretEnc, []byte("secret again"))
	must(t, err)
	if !ok {
		t.Error("Reseal of the webhook's secret: false, want true")
	}
	if _, err := s.Reseal(ctx, "users", 1, []byte("a"), []byte("b")); err == nil {
		t.Error("Reseal of users succeeded")
	}
	testSealedSessions(t, s, r.ID)

	got, err := s.ListKeys(ctx, &r.ID)
	must(t, err)
	want := keys[:2]
	want[0].PrivateEnc = []byte("again 1")
	same(t, "the keys after Reseal: the value, and nothing else", want, got)
	h, err := s.GetWebhook(ctx, hook.ID)
	must(t, err)
	hook.SecretEnc = []byte("secret again")
	same(t, "the webhook after Reseal: the secret, and nothing else, its revision included", hook, *h)

	errBoom := errors.New("boom")
	err = s.Tx(ctx, func(w index.Writer) error {
		ok, err := w.Reseal(ctx, index.SealedKeys, keys[2].ID, []byte("sealed 3"), []byte("rolled back"))
		if err != nil || !ok {
			t.Errorf("Reseal in a transaction: %v, %v", ok, err)
		}
		return errBoom
	})
	wantErr(t, "Tx", err, errBoom)
	server, err := s.ListKeys(ctx, nil)
	must(t, err)
	same(t, "the server key after a Reseal rolled back", "sealed 3", string(server[0].PrivateEnc))
}

// testSealedSessions: ListSealed of SealedSessions answers the open uploads of the facade, each
// with its key as the additional data and its data key, sessions.files enc.key, as the value,
// none for an upload that does not encrypt; never a session of maven, nor one that is not open.
// Reseal refuses them: an upload in flight is not sealed again.
func testSealedSessions(t *testing.T, s index.Store, repoID int64) {
	ctx := t.Context()
	var ids []int64
	for _, sess := range []index.Session{
		{Key: "blob-upload:u1", State: "open", Files: json.RawMessage(`{"name": "a", "enc": {"key": "c2VhbGVkIGtleQ==", "nonce": "bg=="}}`)},
		{Key: "com.example:lib:1.0", State: "open", Files: json.RawMessage(`[{"name": "lib-1.0.jar"}]`)},
		{Key: "blob-upload:u2", State: "open", Files: json.RawMessage(`{"name": "b"}`)},
		{Key: "blob-upload:u3", State: "committed", Files: json.RawMessage(`{"name": "c", "enc": {"key": "b2xk"}}`)},
	} {
		sess.RepoID, sess.Principal, sess.ExpiresAt = repoID, "user:alice", at(3600)
		must(t, s.UpsertSession(ctx, &sess))
		ids = append(ids, sess.ID)
	}
	got, err := s.ListSealed(ctx, index.SealedSessions, 0, 100)
	must(t, err)
	same(t, "the open uploads", []index.Sealed{
		{Table: index.SealedSessions, ID: ids[0], AAD: "blob-upload:u1", Value: []byte("sealed key")},
		{Table: index.SealedSessions, ID: ids[2], AAD: "blob-upload:u2"},
	}, got)
	got, err = s.ListSealed(ctx, index.SealedSessions, ids[0], 1)
	must(t, err)
	same(t, "the open uploads after the first", 1, len(got))
	if _, err := s.Reseal(ctx, index.SealedSessions, ids[0], []byte("sealed key"), []byte("again")); err == nil {
		t.Error("Reseal of an upload session succeeded")
	}
}

// testSealingKey: the row of sealing_key is inserted when there is none, replaced over the id it
// names and never over another; a put in a transaction that rolls back is no put; SetAt is the
// database's time.
func testSealingKey(t *testing.T, s index.Store) {
	ctx := t.Context()
	wantErr(t, "GetSealingKey of none", second(s.GetSealingKey(ctx)), index.ErrNotFound)
	wantErr(t, "PutSealingKey over an id where there is no row",
		s.PutSealingKey(ctx, &index.SealingKey{KeyID: "1111111111111111", Source: index.SealingRewrap, Host: "h"}, "0000000000000000"),
		index.ErrConflict)

	before, err := s.DatabaseTime(ctx)
	must(t, err)
	first := index.SealingKey{KeyID: "0123456789abcdef", Source: index.SealingStart, Host: "serve-1"}
	must(t, s.PutSealingKey(ctx, &first, ""))
	if first.SetAt.Before(before.Add(-sealingClockSlack)) || first.SetAt.After(before.Add(sealingClockSlack)) {
		t.Errorf("SetAt %v, want the database's time, about %v", first.SetAt, before)
	}
	got, err := s.GetSealingKey(ctx)
	must(t, err)
	same(t, "GetSealingKey", first, *got)
	for _, old := range []string{"", "fedcba9876543210"} {
		other := index.SealingKey{KeyID: "fedcba9876543210", Source: index.SealingRewrap, Host: "admin-1"}
		wantErr(t, "PutSealingKey over "+strconv.Quote(old), s.PutSealingKey(ctx, &other, old), index.ErrConflict)
	}

	errBoom := errors.New("boom")
	err = s.Tx(ctx, func(w index.Writer) error {
		must(t, w.PutSealingKey(ctx, &index.SealingKey{KeyID: "fedcba9876543210", Source: index.SealingRewrap, Host: "admin-1",
			AuditID: ptr(int64(7))}, first.KeyID))
		return errBoom
	})
	wantErr(t, "Tx", err, errBoom)
	got, err = s.GetSealingKey(ctx)
	must(t, err)
	same(t, "after the rollback", first, *got)

	next := index.SealingKey{KeyID: "fedcba9876543210", Source: index.SealingRewrap, Host: "admin-1", AuditID: ptr(int64(7))}
	must(t, s.PutSealingKey(ctx, &next, first.KeyID))
	got, err = s.GetSealingKey(ctx)
	must(t, err)
	same(t, "the sealing key switched", next, *got)
}

// sealingClockSlack bounds how far SetAt may be from a read of the database's time before it.
const sealingClockSlack = 5 * time.Second

// testSealingKeyRace: processes that start together with one key each and insert it at once
// leave one row, of the one whose put succeeded; the others get ErrConflict.
func testSealingKeyRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	const replicas = 8
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Go(func() {
			k := index.SealingKey{KeyID: "000000000000000" + strconv.Itoa(i), Source: index.SealingStart, Host: "r" + strconv.Itoa(i)}
			errs[i] = s.PutSealingKey(ctx, &k, "")
		})
	}
	wg.Wait()
	stored, err := s.GetSealingKey(ctx)
	must(t, err)
	winners := 0
	for i, err := range errs {
		if err != nil {
			wantErr(t, "PutSealingKey of replica "+strconv.Itoa(i), err, index.ErrConflict)
			continue
		}
		winners++
		if stored.Host != "r"+strconv.Itoa(i) {
			t.Errorf("replica %d recorded its key, the stored one is %s's", i, stored.Host)
		}
	}
	if winners != 1 {
		t.Fatalf("%d replicas recorded their key, want 1", winners)
	}
}

// testSnapshotNoSealingKey: a database without a sealing key exports none, nor a wrapped master
// key (#1388), and a snapshot without one (a database before 0040) imports none: a restore into an
// empty database chooses no key.
func testSnapshotNoSealingKey(t *testing.T, open func(*testing.T) index.Store) {
	ctx := t.Context()
	src := open(t)
	newRepo(t, src, "raw", "assets")
	cp := export(t, src)
	if cp.SealingKey != nil || len(cp.WrappedMasterKeys) != 0 {
		t.Fatalf("the sealing key and the wrapped master keys of a database without them: %+v, %+v", cp.SealingKey,
			cp.WrappedMasterKeys)
	}
	dst := open(t)
	must(t, snapshotter(t, dst).ImportControlPlane(ctx, cp, index.ImportOptions{}))
	wantErr(t, "the sealing key after the import", second(dst.GetSealingKey(ctx)), index.ErrNotFound)
}
