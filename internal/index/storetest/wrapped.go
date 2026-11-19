package storetest

import (
	"errors"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The data keys of the master key in a KMS (#1388; docs/spec/index-schema.md, A master key in a
// KMS): the rows of wrapped_master_keys, and the ids a replica holds written again after an
// unwrap.

// testWrappedMasterKeys: PutWrappedMasterKey inserts a row where there is none, at the database's
// time, and replaces its ciphertext only where the row still holds the one read, leaving its KMS
// key and CreatedAt; an insert over a row, a replace over another ciphertext or of a row that is
// not there is ErrConflict and changes nothing, nor does a transaction that rolls back;
// ListWrappedMasterKeys answers the rows by id; DeleteWrappedMasterKey deletes one, and one that
// is not there is ErrNotFound.
func testWrappedMasterKeys(t *testing.T, s index.Store) {
	ctx := t.Context()
	list := func() []index.WrappedMasterKey {
		t.Helper()
		rows, err := s.ListWrappedMasterKeys(ctx)
		must(t, err)
		return rows
	}
	same(t, "the rows of an empty table", 0, len(list()))

	before, err := s.DatabaseTime(ctx)
	must(t, err)
	b := index.WrappedMasterKey{KeyID: "fedcba9876543210", KMS: "gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k",
		Wrapped: []byte("ciphertext b"), AuditID: ptr(int64(3))}
	must(t, s.PutWrappedMasterKey(ctx, &b, nil))
	if b.CreatedAt.Before(before.Add(-sealingClockSlack)) || b.CreatedAt.After(before.Add(sealingClockSlack)) ||
		!b.WrappedAt.Equal(b.CreatedAt) {
		t.Errorf("CreatedAt %v, WrappedAt %v: want the database's time, about %v, for both", b.CreatedAt, b.WrappedAt, before)
	}
	a := index.WrappedMasterKey{KeyID: "0123456789abcdef", KMS: "awskms:///arn:aws:kms:eu-west-1:111122223333:key/k",
		Wrapped: []byte("ciphertext a")}
	must(t, s.PutWrappedMasterKey(ctx, &a, nil))
	same(t, "the rows by id", []index.WrappedMasterKey{a, b}, list())

	again := a
	again.Wrapped = []byte("another key")
	wantErr(t, "an insert over a row", s.PutWrappedMasterKey(ctx, &again, nil), index.ErrConflict)
	wantErr(t, "a replace over another ciphertext", s.PutWrappedMasterKey(ctx, &again, []byte("not this")), index.ErrConflict)
	none := index.WrappedMasterKey{KeyID: "1111111111111111", KMS: a.KMS, Wrapped: []byte("x")}
	wantErr(t, "a replace of no row", s.PutWrappedMasterKey(ctx, &none, []byte("x")), index.ErrConflict)
	errBoom := errors.New("boom")
	err = s.Tx(ctx, func(w index.Writer) error {
		k := index.WrappedMasterKey{KeyID: a.KeyID, KMS: a.KMS, Wrapped: []byte("rolled back")}
		must(t, w.PutWrappedMasterKey(ctx, &k, a.Wrapped))
		must(t, w.DeleteWrappedMasterKey(ctx, b.KeyID))
		return errBoom
	})
	wantErr(t, "Tx", err, errBoom)
	same(t, "the rows after a rollback", []index.WrappedMasterKey{a, b}, list())

	rewrapped := index.WrappedMasterKey{KeyID: a.KeyID, KMS: "awskms:///arn:aws:kms:eu-west-1:111122223333:key/other",
		Wrapped: []byte("ciphertext a, again"), AuditID: ptr(int64(9))}
	must(t, s.PutWrappedMasterKey(ctx, &rewrapped, a.Wrapped))
	want := a
	want.Wrapped, want.WrappedAt, want.AuditID = rewrapped.Wrapped, rewrapped.WrappedAt, rewrapped.AuditID
	same(t, "the rows after a wrap again", []index.WrappedMasterKey{want, b}, list())

	must(t, s.DeleteWrappedMasterKey(ctx, b.KeyID))
	same(t, "the rows after a delete", []index.WrappedMasterKey{want}, list())
	wantErr(t, "a delete of no row", s.DeleteWrappedMasterKey(ctx, b.KeyID), index.ErrNotFound)
}

// testReplicaMasterKeys: SetReplicaMasterKeys writes the ids of a replica's row, none as NULL,
// and a row that is gone is ErrNotFound.
func testReplicaMasterKeys(t *testing.T, s index.Store) {
	ctx := t.Context()
	r := index.Replica{ID: "r", Host: "host-r", PID: 7, Version: "v0.1.0", StartedAt: at(0), MasterKeys: []string{"0123456789abcdef"}}
	must(t, s.StartReplica(ctx, &r, at(-120)))
	held := func() []string {
		t.Helper()
		rows, err := s.ListReplicas(ctx)
		must(t, err)
		return rows[0].MasterKeys
	}
	must(t, s.SetReplicaMasterKeys(ctx, r.ID, []string{"0123456789abcdef", "fedcba9876543210"}))
	same(t, "the ids after an unwrap", []string{"0123456789abcdef", "fedcba9876543210"}, held())
	must(t, s.SetReplicaMasterKeys(ctx, r.ID, nil))
	if ids := held(); ids != nil {
		t.Errorf("the ids after none = %q, want nil", ids)
	}
	must(t, s.StopReplica(ctx, r.ID))
	wantErr(t, "the ids of a row that is gone", s.SetReplicaMasterKeys(ctx, r.ID, []string{"0123456789abcdef"}), index.ErrNotFound)
}
