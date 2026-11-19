package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// The sealing key at the start of a process (docs/spec/index-schema.md, The master key and its
// replacement, #1288): the row of sealing_key names the master key that every process seals
// with, and a process that does not hold it seals nothing.

// sealingStore is the part of index.Store that the start reads and writes of the sealing key.
type sealingStore interface {
	GetSealingKey(ctx context.Context) (*index.SealingKey, error)
	PutSealingKey(ctx context.Context, k *index.SealingKey, old string) error
	ListSealed(ctx context.Context, table string, afterID int64, limit int) ([]index.Sealed, error)
	wrappedReader
}

// rewrapHint is what a process that holds several keys on a database that names none says.
const rewrapHint = "name one with lajstrom admin key rewrap --to <id>"

// sealingAtStart reads the row of sealing_key and makes the key it names the one m seals with,
// once, at the start of serve and of every command that reads the master keys but rewrap and
// snapshot restore: after the migrations, before the process writes its replicas row, seals
// anything or listens. A row that names a key m does not hold is an error that names the id and
// the ids m holds, and so the process stops: serve does not start, a command exits 1 before it
// writes. Without a row (the first start of a release with 0040, or a new database) a process that
// holds one key opens the first key of the index with it, when there is one, records it, source
// start, where there is still no row, and reads the row again: of processes that start together
// one records its key, and one that holds another stops. One that holds several records nothing
// and stops: which of them seals is the operator's choice (rewrap --to), never the order of the
// files.
//
// A data key of keys.masterKeyKMS that the KMS has not unwrapped yet, which only serve holds
// (#1388), stops nothing: a row that names it leaves the process sealing nothing until a beat
// after the unwrap, and so does a database without a row whose sole key it is, which that beat
// records.
func sealingAtStart(ctx context.Context, st sealingStore, m *keys.Master, host string) error {
	row, err := st.GetSealingKey(ctx)
	if errors.Is(err, index.ErrNotFound) {
		if soleKeyPending(m) {
			return nil
		}
		if err := recordSealing(ctx, st, m, host); err != nil {
			return err
		}
		row, err = st.GetSealingKey(ctx)
	}
	if err != nil {
		return fmt.Errorf("the sealing key of the index: %w", err)
	}
	if !m.SealWith(row.KeyID) && !m.IsPending(row.KeyID) {
		return fmt.Errorf("the index seals with master key %s, which this process does not hold; it holds %s: add that key to "+
			"keys.masterKeyDir, or name one this process holds with lajstrom admin key rewrap --to <id>",
			row.KeyID, heldIDs(m))
	}
	return nil
}

// recordSealing records the one key m holds as the sealing key, source start, where there is no
// row, once it has opened the first key of the index. A conflict is another process's row, which
// the caller reads.
func recordSealing(ctx context.Context, st sealingStore, m *keys.Master, host string) error {
	ids := m.IDs()
	if len(ids)+len(m.Pending()) > 1 {
		return fmt.Errorf("no master key seals yet: %s; this process holds %s", rewrapHint, heldIDs(m))
	}
	first, err := st.ListSealed(ctx, index.SealedKeys, 0, 1)
	if err != nil {
		return fmt.Errorf("the first key of the index: %w", err)
	}
	if len(first) == 1 {
		if _, _, err := m.OpenKey(first[0].AAD, first[0].Value); err != nil {
			return fmt.Errorf("the master key %s does not open the first key of the index (keys id %d, fingerprint %s): it is not "+
				"the master key of this database; start with the one that sealed it (docs/operations.md, The master key)",
				ids[0], first[0].ID, first[0].AAD)
		}
	}
	err = st.PutSealingKey(ctx, &index.SealingKey{KeyID: ids[0], Source: index.SealingStart, Host: host}, "")
	if err != nil && !errors.Is(err, index.ErrConflict) {
		return fmt.Errorf("the sealing key of the index: %w", err)
	}
	return nil
}

// heldIDs is the ids of the keys m holds, for an error: "0123456789abcdef, fedcba9876543210",
// and those it has not unwrapped yet, so marked.
func heldIDs(m *keys.Master) string {
	ids := m.IDs()
	for _, id := range m.Pending() {
		ids = append(ids, id+" (not unwrapped yet)")
	}
	return strings.Join(ids, ", ")
}

// soleKeyPending reports whether the one key of m is a data key not unwrapped yet.
func soleKeyPending(m *keys.Master) bool { return len(m.IDs()) == 0 && len(m.Pending()) == 1 }
