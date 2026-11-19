package storetest

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The refresh of OIDC logins (#1282; docs/spec/index-schema.md, oidc_refresh): a row per login,
// joined to its token at the mint, claimed by one replica at a time, its outcome written under
// the claim only.

// refreshAAD is the additional data of the refresh token of row id.
func refreshAAD(id int64) string { return "oidc_refresh:" + strconv.FormatInt(id, 10) }

// testOIDCRefresh: a row per login, unique; JoinOIDCRefresh joins the row of a login once, to a
// token no other row has; ClaimOIDCRefresh takes the due rows whose claim is free or ended, by
// due time; FinishOIDCRefresh writes only under the claim it was given, keeps the sealed value
// when it has none, and frees the claim; ListSealed and Reseal see the sealed values;
// DeleteOIDCRefresh and ClearOIDCRefresh delete, and an identity deleted takes its rows.
func testOIDCRefresh(t *testing.T, s index.Store) {
	ctx := t.Context()
	alice := newUser(t, s, "alice")
	ident := index.Identity{UserID: alice.ID, Issuer: "https://idp", Subject: "a"}
	must(t, s.CreateIdentity(ctx, &ident))
	tok1 := index.Token{Kind: "pat", UserID: &alice.ID, Name: "oidc 1", Hash: "h1"}
	tok2 := index.Token{Kind: "pat", UserID: &alice.ID, Name: "oidc 2", Hash: "h2"}
	must(t, s.CreateToken(ctx, &tok1))
	must(t, s.CreateToken(ctx, &tok2))

	// Three logins' rows before their tokens, each due 10 minutes after its callback.
	rows := make([]index.OIDCRefresh, 3)
	for i := range rows {
		rows[i] = index.OIDCRefresh{LoginID: ptr(int64(101 + i)), IdentityID: ident.ID, Sealed: []byte("sealed " + strconv.Itoa(i)),
			CreatedAt: at(i), NextAt: at(600 + i)}
		must(t, s.CreateOIDCRefresh(ctx, &rows[i]))
	}
	same(t, "RefreshedAt of a new row", at(0), rows[0].RefreshedAt)
	dup := index.OIDCRefresh{LoginID: ptr(int64(101)), IdentityID: ident.ID, Sealed: []byte("x"), NextAt: at(600)}
	wantErr(t, "CreateOIDCRefresh of a login twice", s.CreateOIDCRefresh(ctx, &dup), index.ErrConflict)

	// The mints join the first two to their tokens; the third login's token is never minted.
	must(t, s.JoinOIDCRefresh(ctx, 101, tok1.ID, at(900)))
	wantErr(t, "JoinOIDCRefresh twice", s.JoinOIDCRefresh(ctx, 101, tok1.ID, at(900)), index.ErrNotFound)
	wantErr(t, "JoinOIDCRefresh of a login without a row", s.JoinOIDCRefresh(ctx, 999, tok2.ID, at(900)), index.ErrNotFound)
	wantErr(t, "JoinOIDCRefresh to a token joined already", s.JoinOIDCRefresh(ctx, 102, tok1.ID, at(900)), index.ErrConflict)
	must(t, s.JoinOIDCRefresh(ctx, 102, tok2.ID, at(950)))

	page, err := s.ListSealed(ctx, index.SealedRefresh, 0, 10)
	must(t, err)
	want := make([]index.Sealed, len(rows))
	for i, r := range rows {
		want[i] = index.Sealed{Table: index.SealedRefresh, ID: r.ID, AAD: refreshAAD(r.ID), Value: r.Sealed}
	}
	same(t, "ListSealed of oidc_refresh", want, page)
	ok, err := s.Reseal(ctx, index.SealedRefresh, rows[0].ID, []byte("sealed 0"), []byte("again 0"))
	if err != nil || !ok {
		t.Errorf("Reseal of a refresh token = %v, %v; want true", ok, err)
	}

	got, err := s.ClaimOIDCRefresh(ctx, "r-a", at(599), at(719), 10)
	must(t, err)
	same(t, "the rows due before any is", 0, len(got))
	got, err = s.ClaimOIDCRefresh(ctx, "r-a", at(910), at(1030), 10)
	must(t, err)
	unjoined, first := rows[2], rows[0]
	unjoined.ClaimedBy, unjoined.ClaimedUntil = "r-a", ptr(at(1030))
	first.LoginID, first.TokenID, first.NextAt, first.Sealed = nil, &tok1.ID, at(900), []byte("again 0")
	first.ClaimedBy, first.ClaimedUntil = "r-a", ptr(at(1030))
	same(t, "the rows claimed: due, by due time", []index.OIDCRefresh{unjoined, first}, got)
	got, err = s.ClaimOIDCRefresh(ctx, "r-b", at(1000), at(1120), 10)
	must(t, err)
	same(t, "another replica's claim while the first lasts", []int64{rows[1].ID}, ids(got, func(r index.OIDCRefresh) int64 { return r.ID }))
	second := got[0]

	// The outcome is written under the claim only.
	stranger := first
	wantErr(t, "FinishOIDCRefresh of another replica", s.FinishOIDCRefresh(ctx, &stranger, "r-b"), index.ErrConflict)
	stranger.ClaimedUntil = ptr(at(1031))
	wantErr(t, "FinishOIDCRefresh of another claim", s.FinishOIDCRefresh(ctx, &stranger, "r-a"), index.ErrConflict)
	first.Sealed, first.RefreshedAt, first.NextAt = []byte("rotated"), at(915), at(1800)
	must(t, s.FinishOIDCRefresh(ctx, &first, "r-a"))
	if first.ClaimedBy != "" || first.ClaimedUntil != nil {
		t.Errorf("the claim after FinishOIDCRefresh: %q, %v", first.ClaimedBy, first.ClaimedUntil)
	}
	first.ClaimedBy, first.ClaimedUntil = "r-a", ptr(at(1030))
	wantErr(t, "FinishOIDCRefresh twice", s.FinishOIDCRefresh(ctx, &first, "r-a"), index.ErrConflict)
	unjoined.Sealed, unjoined.NextAt, unjoined.Failures = nil, at(970), 1 // an error: the value stays
	must(t, s.FinishOIDCRefresh(ctx, &unjoined, "r-a"))

	// The claim of r-b on the second row ended: it takes it again, and its earlier claim is gone.
	got, err = s.ClaimOIDCRefresh(ctx, "r-b", at(1200), at(1320), 10)
	must(t, err)
	unjoined.Sealed, unjoined.ClaimedBy, unjoined.ClaimedUntil = []byte("sealed 2"), "r-b", ptr(at(1320))
	again := second
	again.ClaimedUntil = ptr(at(1320))
	same(t, "the rows claimed after a claim ended", []index.OIDCRefresh{again, unjoined}, got)
	wantErr(t, "FinishOIDCRefresh under a claim that ended", s.FinishOIDCRefresh(ctx, &second, "r-b"), index.ErrConflict)
	page, err = s.ListSealed(ctx, index.SealedRefresh, 0, 10)
	must(t, err)
	same(t, "the values after the refreshes", []string{"rotated", "sealed 1", "sealed 2"},
		[]string{string(page[0].Value), string(page[1].Value), string(page[2].Value)})

	must(t, s.DeleteOIDCRefresh(ctx, rows[2].ID))
	wantErr(t, "DeleteOIDCRefresh twice", s.DeleteOIDCRefresh(ctx, rows[2].ID), index.ErrNotFound)
	bob := newUser(t, s, "bob")
	other := index.Identity{UserID: bob.ID, Issuer: "https://idp", Subject: "b"}
	must(t, s.CreateIdentity(ctx, &other))
	gone := index.OIDCRefresh{LoginID: ptr(int64(201)), IdentityID: other.ID, Sealed: []byte("bob's"), NextAt: at(600)}
	must(t, s.CreateOIDCRefresh(ctx, &gone))
	must(t, s.DeleteIdentity(ctx, other.ID))
	n, err := s.ClearOIDCRefresh(ctx)
	must(t, err)
	same(t, "ClearOIDCRefresh after an identity took its row", 2, n)
	page, err = s.ListSealed(ctx, index.SealedRefresh, 0, 10)
	must(t, err)
	same(t, "the rows after ClearOIDCRefresh", 0, len(page))
}

// testOIDCRefreshSeal: the row of a login is inserted and its value sealed with its id in one
// transaction (CreateOIDCRefresh, then Reseal), and a rollback leaves no row.
func testOIDCRefreshSeal(t *testing.T, s index.Store) {
	ctx := t.Context()
	u := newUser(t, s, "alice")
	ident := index.Identity{UserID: u.ID, Issuer: "https://idp", Subject: "a"}
	must(t, s.CreateIdentity(ctx, &ident))
	errBoom := errors.New("boom")
	for _, fail := range []bool{true, false} {
		err := s.Tx(ctx, func(w index.Writer) error {
			r := index.OIDCRefresh{LoginID: ptr(int64(7)), IdentityID: ident.ID, Sealed: []byte{0}, NextAt: at(600)}
			if err := w.CreateOIDCRefresh(ctx, &r); err != nil {
				return err
			}
			ok, err := w.Reseal(ctx, index.SealedRefresh, r.ID, []byte{0}, []byte(refreshAAD(r.ID)))
			if err != nil || !ok {
				t.Errorf("Reseal of the new row = %v, %v; want true", ok, err)
			}
			if fail {
				return errBoom
			}
			return nil
		})
		if fail {
			wantErr(t, "Tx", err, errBoom)
		} else {
			must(t, err)
		}
	}
	page, err := s.ListSealed(ctx, index.SealedRefresh, 0, 10)
	must(t, err)
	if len(page) != 1 || string(page[0].Value) != page[0].AAD {
		t.Errorf("the rows after a rollback and a commit: %+v, want one sealed with its own id", page)
	}
}

// oidcRefreshClaimDeadline bounds testOIDCRefreshClaimRace.
const oidcRefreshClaimDeadline = 10 * time.Second

// testOIDCRefreshClaimRace: replicas that claim the due rows at once never take one row twice.
func testOIDCRefreshClaimRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	u := newUser(t, s, "alice")
	ident := index.Identity{UserID: u.ID, Issuer: "https://idp", Subject: "a"}
	must(t, s.CreateIdentity(ctx, &ident))
	const rows, replicas = 30, 6
	for i := range rows {
		r := index.OIDCRefresh{LoginID: ptr(int64(i + 1)), IdentityID: ident.ID, Sealed: []byte("s"), NextAt: at(i)}
		must(t, s.CreateOIDCRefresh(ctx, &r))
	}
	var (
		mu      sync.Mutex
		claimed = map[int64]string{}
		wg      sync.WaitGroup
	)
	deadline := time.Now().Add(oidcRefreshClaimDeadline)
	for r := range replicas {
		wg.Go(func() {
			replica := "r" + strconv.Itoa(r)
			for {
				mu.Lock()
				done := len(claimed) == rows
				mu.Unlock()
				if done || time.Now().After(deadline) {
					return
				}
				got, err := s.ClaimOIDCRefresh(ctx, replica, at(100), at(220), 4)
				if err != nil {
					if errors.Is(err, index.ErrUnavailable) { // SQLite busy: try again
						time.Sleep(time.Millisecond)
						continue
					}
					t.Error(err)
					return
				}
				if len(got) == 0 {
					time.Sleep(webhookClaimPause)
					continue
				}
				mu.Lock()
				for _, row := range got {
					if prev, ok := claimed[row.ID]; ok {
						t.Errorf("oidc_refresh %d claimed by %s and %s", row.ID, prev, replica)
					}
					claimed[row.ID] = replica
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	same(t, "the rows claimed", rows, len(claimed))
}
