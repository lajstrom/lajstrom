package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// The data keys of keys.masterKeyKMS at the start of a process (docs/spec/index-schema.md, A
// master key in a KMS, #1388): each entry's row, checked against the entry, then unwrapped by its
// KMS key.

// wrappedReader is the part of index.Store that reads the rows of the data keys.
type wrappedReader interface {
	ListWrappedMasterKeys(ctx context.Context) ([]index.WrappedMasterKey, error)
}

// dataKey is an entry of keys.masterKeyKMS with its row and the client of its KMS key.
type dataKey struct {
	key config.KMSKey
	row index.WrappedMasterKey
	kms keys.MasterKMS
}

// masterClients returns the KMS clients that wrap the data keys of the master key, by scheme,
// configured as kmsClients configures them.
func masterClients(lookupEnv func(string) (string, bool), log *slog.Logger) map[string]keys.MasterKMS {
	out := map[string]keys.MasterKMS{}
	for scheme, c := range kmsClients(lookupEnv, log) {
		if w, ok := c.(keys.MasterKMS); ok {
			out[scheme] = w
		}
	}
	return out
}

// dataKeyIDs returns the ids of the entries of keys.masterKeyKMS.
func dataKeyIDs(entries []config.MasterKeyKMS) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

// errNoRow is the error of an entry of keys.masterKeyKMS without a row in wrapped_master_keys.
var errNoRow = errors.New("no row")

// dataKeysOf reads the rows of wrapped_master_keys and returns the entries with theirs, in the
// order of the configuration. An entry without a row, or whose row is under another KMS key, is an
// error of the configuration that names its id and the KMS keys, never a ciphertext, and wraps
// errNoRow for the first: a process holds only the keys its configuration names, by id and KMS
// key, and a row the database changed is never asked for.
func dataKeysOf(ctx context.Context, st wrappedReader, entries []config.MasterKeyKMS, clients map[string]keys.MasterKMS) (
	[]dataKey, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	rows, err := st.ListWrappedMasterKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("the wrapped master keys of the index: %w", err)
	}
	byID := map[string]index.WrappedMasterKey{}
	for _, r := range rows {
		byID[r.KeyID] = r
	}
	out := make([]dataKey, 0, len(entries))
	var errs []error
	for _, e := range entries {
		key, err := config.ParseMasterKMSKey(e.KMS) // Load has checked it
		if err != nil {
			return nil, fmt.Errorf("keys.masterKeyKMS: %w", err)
		}
		row, ok := byID[e.ID]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("keys.masterKeyKMS: master key %s has no row in wrapped_master_keys of this database: %w; "+
				"make it with lajstrom admin key wrap --kms %s, or remove the entry", e.ID, errNoRow, e.KMS))
			continue
		case row.KMS != e.KMS:
			errs = append(errs, fmt.Errorf("keys.masterKeyKMS: master key %s is wrapped by %s in this database, not by %s, which the "+
				"entry names: it is not asked for", e.ID, row.KMS, e.KMS))
			continue
		}
		c := clients[key.Scheme]
		if c == nil {
			return nil, fmt.Errorf("keys.masterKeyKMS: master key %s: this binary has no client for %s", e.ID, key.Scheme)
		}
		out = append(out, dataKey{key: key, row: row, kms: c})
	}
	return out, errors.Join(errs...)
}

// unwrap has the KMS unwrap d into m. Its error names the id and the KMS key and says whether
// the KMS refused, did not answer, or answered another key; it shows no key and no ciphertext.
func (d dataKey) unwrap(ctx context.Context, m *keys.Master) error {
	err := m.Unwrap(ctx, d.kms, d.key, d.row)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, keys.ErrOtherDataKey):
		return fmt.Errorf("keys.masterKeyKMS: master key %s: the KMS key %s answered another key, which is not held: the row of "+
			"wrapped_master_keys was changed: %w", d.row.KeyID, d.key, err)
	case errors.Is(err, keys.ErrKMSUnavailable):
		return fmt.Errorf("keys.masterKeyKMS: master key %s: the KMS key %s did not answer: %w", d.row.KeyID, d.key, err)
	}
	return fmt.Errorf("keys.masterKeyKMS: master key %s: the KMS key %s refused to unwrap it: %w", d.row.KeyID, d.key, err)
}

// unwrapAll has the KMS unwrap every data key of the configuration into m, at the start of a
// command, before it writes: one that does not open stops it, exit status 1 (#1388), since a
// command with a key missing would count the values under it as under none. noRow leaves the
// entries without a row as they are, for a restore, which may take them from its snapshot.
func (a *app) unwrapAll(ctx context.Context, st wrappedReader, m *keys.Master, noRow bool) error {
	if m == nil || len(a.cfg.Keys.MasterKeyKMS) == 0 {
		return nil
	}
	ds, err := dataKeysOf(ctx, st, a.cfg.Keys.MasterKeyKMS, a.masterKMS())
	if err != nil && (!noRow || !onlyNoRow(err)) {
		return err
	}
	for _, d := range ds {
		if err := d.unwrap(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// onlyNoRow reports whether err, of dataKeysOf, is of entries without a row only.
func onlyNoRow(err error) bool {
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		return errors.Is(err, errNoRow)
	}
	for _, e := range joined.Unwrap() {
		if !errors.Is(e, errNoRow) {
			return false
		}
	}
	return true
}

// masterKMS returns the clients of the data keys: a.kmsClients when a test set it, masterClients
// otherwise.
func (a *app) masterKMS() map[string]keys.MasterKMS {
	if a.wrapKMS != nil {
		return a.wrapKMS
	}
	return masterClients(a.lookupEnv, a.log)
}
