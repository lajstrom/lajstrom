package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TestCountSealedRefreshLost: a refresh token under no key the command holds is named by its row,
// oidc_refresh id <n>, never by its value, and counted as lost.
func TestCountSealedRefreshLost(t *testing.T) {
	st := newSealingStore(t)
	plain := map[string]string{}
	ids := fillRefreshTokens(t, st, plain, thirdMasterKey)
	c, err := countSealed(t.Context(), st, heldOf(t, testMasterKey, otherMasterKey))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Lost) != 1 || c.Lost[0] != (lostValue{Table: index.SealedRefresh, ID: ids[0]}) ||
		c.Lost[0].String() != fmt.Sprintf("oidc_refresh id %d", ids[0]) || lostCounts(c.Lost) != (keyCounts{Refresh: 1}) {
		t.Errorf("Lost = %+v", c.Lost)
	}
}

// rotating is a store whose first ListSealed of oidc_refresh, after its read, has serve's refresh
// write a rotated refresh token of the first row under the old key, under a claim: a rotation
// between the read of a batch and its write.
type rotating struct {
	store
	hex  string
	once sync.Once
}

func (s *rotating) ListSealed(ctx context.Context, table string, afterID int64, limit int) ([]index.Sealed, error) {
	page, err := s.store.ListSealed(ctx, table, afterID, limit)
	if table == index.SealedRefresh && len(page) > 0 && err == nil {
		s.once.Do(func() { err = s.rotate(ctx, page[0]) })
	}
	return page, err
}

func (s *rotating) rotate(ctx context.Context, v index.Sealed) error {
	far := time.Now().Add(24 * time.Hour)
	rows, err := s.ClaimOIDCRefresh(ctx, "serve-1", far, far.Add(time.Minute), 10)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.ID != v.ID {
			continue
		}
		if r.Sealed, err = sealOne(s.hex, v.AAD, "a rotated refresh token"); err != nil {
			return err
		}
		return s.FinishOIDCRefresh(ctx, &r, "serve-1")
	}
	return fmt.Errorf("oidc_refresh %d not claimed", v.ID)
}

// TestResealRefreshRotated: a refresh token that the IdP rotated between the read of its batch and
// the write is left by that batch, read again after the last one, and sealed again then: the
// rotated token is under the new key at the end.
func TestResealRefreshRotated(t *testing.T) {
	ctx := t.Context()
	f := newRewrapFixture(t)
	m := heldOf(t, testMasterKey, otherMasterKey)
	m.SealWith(f.b)
	done, err := reseal(ctx, &rotating{store: f.st, hex: testMasterKey}, m, f.b)
	if err != nil || done != (keyCounts{Keys: 3, Webhooks: 2, Refresh: 1}) {
		t.Fatalf("reseal = %+v, %v; want 3 keys, 2 webhooks and the rotated refresh token", done, err)
	}
	page, err := f.st.ListSealed(ctx, index.SealedRefresh, f.refreshIDs[0]-1, 1)
	if err != nil || len(page) != 1 {
		t.Fatalf("the refresh token: %+v, %v", page, err)
	}
	plain, by, err := m.OpenKey(auth.RefreshAAD(f.refreshIDs[0]), page[0].Value)
	if err != nil || by != f.b || string(plain) != "a rotated refresh token" {
		t.Errorf("the rotated refresh token: %q under %s, %v; want it under %s", plain, by, err, f.b)
	}
}
