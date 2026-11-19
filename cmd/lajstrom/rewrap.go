package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// The rewrap of the values that the master keys seal (docs/spec/index-schema.md, The master key
// and its replacement, #1288): the guard, the count of the values under each key, and the batches
// that seal them again under the key the index names. lajstrom admin key rewrap runs them
// (admin_key_rewrap.go).

// rewrapBatch is how many rows of a table one transaction of a rewrap seals again.
const rewrapBatch = 100

// sealedTables are the columns a rewrap seals again, in its order: keys.private_enc, then
// webhooks.secret_enc.
var sealedTables = []string{index.SealedKeys, index.SealedWebhooks}

// noKey is the key id of the census under which it counts the upload sessions that no key held
// opens.
const noKey = "none"

// keyCounts are values by table: of keys and of webhooks.
type keyCounts struct {
	Keys     int `json:"keys"`
	Webhooks int `json:"webhooks"`
}

// add counts n values of table.
func (c *keyCounts) add(table string, n int) {
	if table == index.SealedKeys {
		c.Keys += n
	} else {
		c.Webhooks += n
	}
}

// lostValue names a value under no master key the process holds, by its row: a key by its id,
// kind and fingerprint, a webhook by its id; never the value.
type lostValue struct {
	Table       string `json:"table"`
	ID          int64  `json:"id"`
	Kind        string `json:"kind,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// String is the row of v as an error names it: "keys id 3 (gpg AB12…)", "webhooks id 2".
func (v lostValue) String() string {
	if v.Table == index.SealedKeys {
		return fmt.Sprintf("keys id %d (%s %s)", v.ID, v.Kind, v.Fingerprint)
	}
	return fmt.Sprintf("%s id %d", v.Table, v.ID)
}

// openSessions are the open upload sessions of the facade under one key: how many, and when the
// last of them expires.
type openSessions struct {
	Open          int       `json:"open"`
	LastExpiresAt time.Time `json:"lastExpiresAt"`
}

// census is what a count of the sealed values found: the values of keys and webhooks under each
// key id; those under no key held, by row; and the open upload sessions of encrypted repos under
// each key id, noKey for those under none.
type census struct {
	Under    map[string]keyCounts
	Lost     []lostValue
	Sessions map[string]openSessions
}

// other returns the counts of c under every key but id.
func (c census) other(id string) map[string]keyCounts {
	out := map[string]keyCounts{}
	for k, n := range c.Under {
		if k != id {
			out[k] = n
		}
	}
	return out
}

// countSealed reads every value of keys.private_enc and webhooks.secret_enc, a page at a time,
// and the data keys of the open uploads of the facade, and opens each with m, its sealing key
// first, to count them under the key that opens each. A value opened is cleared at once.
func countSealed(ctx context.Context, st store, m *keys.Master) (census, error) {
	c := census{Under: map[string]keyCounts{}, Sessions: map[string]openSessions{}}
	for _, table := range sealedTables {
		err := eachSealed(ctx, st, table, func(page []index.Sealed) error {
			for _, v := range page {
				plain, id, err := m.OpenKey(v.AAD, v.Value)
				clear(plain)
				if err != nil {
					c.Lost = append(c.Lost, lostValue{Table: table, ID: v.ID, Fingerprint: fingerprintOf(v)})
					continue
				}
				n := c.Under[id]
				n.add(table, 1)
				c.Under[id] = n
			}
			return nil
		})
		if err != nil {
			return census{}, err
		}
	}
	if err := nameLost(ctx, st, c.Lost); err != nil {
		return census{}, err
	}
	return c, countSessions(ctx, st, m, &c)
}

// fingerprintOf is the fingerprint of the key of v, its additional data; "" for a webhook.
func fingerprintOf(v index.Sealed) string {
	if v.Table == index.SealedKeys {
		return v.AAD
	}
	return ""
}

// eachSealed runs fn on the values of table, a page of rewrapBatch at a time, by id.
func eachSealed(ctx context.Context, st store, table string, fn func([]index.Sealed) error) error {
	for after := int64(0); ; {
		page, err := st.ListSealed(ctx, table, after, rewrapBatch)
		if err != nil {
			return fmt.Errorf("the sealed values of %s: %w", table, err)
		}
		if len(page) == 0 {
			return nil
		}
		if err := fn(page); err != nil {
			return err
		}
		if len(page) < rewrapBatch {
			return nil
		}
		after = page[len(page)-1].ID
	}
}

// nameLost fills the kind of the keys among lost, from the keys of the server and of every repo.
func nameLost(ctx context.Context, st store, lost []lostValue) error {
	if !slices.ContainsFunc(lost, func(v lostValue) bool { return v.Table == index.SealedKeys }) {
		return nil
	}
	repos, err := st.ListRepos(ctx, "")
	if err != nil {
		return err
	}
	kinds := map[int64]string{}
	owners := []*int64{nil}
	for _, r := range repos {
		owners = append(owners, &r.ID)
	}
	for _, owner := range owners {
		ks, err := st.ListKeys(ctx, owner)
		if err != nil {
			return err
		}
		for _, k := range ks {
			kinds[k.ID] = k.Kind
		}
	}
	for i := range lost {
		if lost[i].Table == index.SealedKeys {
			lost[i].Kind = kinds[lost[i].ID]
		}
	}
	return nil
}

// countSessions counts the open upload sessions of the facade that hold a sealed data key into
// c, by the key that opens it, with when the last of each expires. They are not sealed again: they
// end, an hour after their last request at the latest.
func countSessions(ctx context.Context, st store, m *keys.Master, c *census) error {
	return eachUpload(ctx, st, func(v index.Sealed) error {
		if v.Value == nil {
			return nil // an upload to a repo that does not encrypt
		}
		plain, id, err := m.OpenKey(v.AAD, v.Value)
		clear(plain)
		if err != nil {
			id = noKey
		}
		s, err := st.GetSessionByID(ctx, v.ID)
		if errors.Is(err, index.ErrNotFound) {
			return nil // it ended in between
		}
		if err != nil {
			return err
		}
		n := c.Sessions[id]
		n.Open++
		if s.ExpiresAt.After(n.LastExpiresAt) {
			n.LastExpiresAt = s.ExpiresAt
		}
		c.Sessions[id] = n
		return nil
	})
}

// eachUpload runs fn on the open uploads of the facade (index.SealedSessions), by id.
func eachUpload(ctx context.Context, st store, fn func(index.Sealed) error) error {
	return eachSealed(ctx, st, index.SealedSessions, func(page []index.Sealed) error {
		for _, v := range page {
			if err := fn(v); err != nil {
				return err
			}
		}
		return nil
	})
}

// rewrapGuard returns what a rewrap to the key to refuses, each a line that names a key by its id
// and a replica by its host, pid and version: that m, the keys of the command, does not hold it,
// and each replica with a heartbeat younger than replicaStaleAfter, on the database's clock, that
// does not list it among its keys, one of the release before 0040 included, which lists none.
func rewrapGuard(ctx context.Context, st store, m *keys.Master, to string) ([]string, error) {
	var refusals []string
	if !m.Holds(to) {
		refusals = append(refusals, fmt.Sprintf("this command does not hold master key %s; it holds %s: add it to keys.masterKeyDir",
			to, heldIDs(m)))
	}
	live, err := liveReplicas(ctx, st)
	if err != nil {
		return nil, err
	}
	for _, r := range live {
		if slices.Contains(r.MasterKeys, to) {
			continue
		}
		holds := "it holds " + strings.Join(r.MasterKeys, ", ")
		if len(r.MasterKeys) == 0 {
			holds = "it lists no master key: a release before 0040, or one without a master key"
		}
		refusals = append(refusals, fmt.Sprintf("the replica on %s (pid %d, %s) does not hold master key %s; %s",
			r.Host, r.PID, r.Version, to, holds))
	}
	return refusals, nil
}

// liveReplicas returns the replicas whose heartbeat is younger than replicaStaleAfter on the
// database's clock.
func liveReplicas(ctx context.Context, st store) ([]index.Replica, error) {
	now, err := st.DatabaseTime(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the database's clock: %w", err)
	}
	rows, err := st.ListReplicas(ctx)
	if err != nil {
		return nil, err
	}
	var live []index.Replica
	for _, r := range rows {
		if r.HeartbeatAt.After(now.Add(-replicaStaleAfter)) {
			live = append(live, r)
		}
	}
	return live, nil
}

// reseal seals again under the key m seals with, to, every value of keys.private_enc and
// webhooks.secret_enc that another key m holds opens: rewrapBatch rows a transaction, each value
// written only where its row still holds the value read (Reseal), a new nonce and the same
// additional data. A value under to is left as it is, and so is one under no key held (the check
// has stopped the run unless --allow-lost). A row changed since its batch read it is read again
// after the last batch, once. It returns what it sealed again, also when it fails part way.
func reseal(ctx context.Context, st store, m *keys.Master, to string) (keyCounts, error) {
	var done keyCounts
	for _, table := range sealedTables {
		var changed []int64
		err := eachSealed(ctx, st, table, func(page []index.Sealed) error {
			n, missed, err := resealBatch(ctx, st, m, to, page)
			done.add(table, n)
			changed = append(changed, missed...)
			return err
		})
		for _, id := range changed {
			if err != nil {
				break
			}
			var page []index.Sealed
			if page, err = st.ListSealed(ctx, table, id-1, 1); err == nil && len(page) == 1 && page[0].ID == id {
				var n int
				n, _, err = resealBatch(ctx, st, m, to, page)
				done.add(table, n)
			}
		}
		if err != nil {
			return done, fmt.Errorf("the rewrap of %s: %w", table, err)
		}
	}
	return done, nil
}

// resealBatch seals again the values of page under another key than to in one transaction, and
// returns how many it wrote and the ids of the rows changed since page was read.
func resealBatch(ctx context.Context, st store, m *keys.Master, to string, page []index.Sealed) (int, []int64, error) {
	var n int
	var changed []int64
	err := st.Tx(ctx, func(w index.Writer) error {
		n, changed = 0, nil // a transaction the database aborts runs again
		for _, v := range page {
			plain, id, err := m.OpenKey(v.AAD, v.Value)
			if err != nil || id == to {
				clear(plain)
				continue
			}
			sealed, err := m.Seal(v.AAD, plain)
			clear(plain)
			if err != nil {
				return err
			}
			ok, err := w.Reseal(ctx, v.Table, v.ID, v.Value, sealed)
			if err != nil {
				return err
			}
			if ok {
				n++
			} else {
				changed = append(changed, v.ID)
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return n, changed, nil
}
