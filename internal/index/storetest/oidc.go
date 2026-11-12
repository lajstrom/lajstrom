package storetest

import (
	"errors"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testIdentities: an identity is one per issuer and subject; SetIdentityGroups replaces its
// groups exactly, stores the times and moves auth_version, or changes nothing; ListIdentityGroups
// has every identity's groups with the user's uid, ordered; a deleted identity takes its groups.
func testIdentities(t *testing.T, s index.Store) {
	ctx := t.Context()
	alice := index.User{Name: "alice", Kind: "oidc", CreatedAt: at(0)}
	bob := index.User{Name: "bob", Kind: "oidc", CreatedAt: at(0)}
	must(t, s.CreateUser(ctx, &alice))
	must(t, s.CreateUser(ctx, &bob))

	wantErr(t, "GetIdentity of none", second(s.GetIdentity(ctx, "https://idp", "a")), index.ErrNotFound)
	ia := index.Identity{UserID: alice.ID, Issuer: "https://idp", Subject: "a", CreatedAt: at(1)}
	must(t, s.CreateIdentity(ctx, &ia))
	same(t, "LoginAt of a new identity", at(1), ia.LoginAt)
	got, err := s.GetIdentity(ctx, "https://idp", "a")
	must(t, err)
	same(t, "GetIdentity", ia, *got)
	dup := index.Identity{UserID: bob.ID, Issuer: "https://idp", Subject: "a"}
	wantErr(t, "CreateIdentity of a taken subject", s.CreateIdentity(ctx, &dup), index.ErrConflict)
	wantErr(t, "GetIdentity of another issuer", second(s.GetIdentity(ctx, "https://other", "a")), index.ErrNotFound)
	ib := index.Identity{UserID: bob.ID, Issuer: "https://other", Subject: "a", CreatedAt: at(2)}
	must(t, s.CreateIdentity(ctx, &ib))

	v0, err := s.AuthVersion(ctx)
	must(t, err)
	must(t, s.SetIdentityGroups(ctx, ia.ID, []string{"team-b", "team-a"}, at(10), at(100)))
	must(t, s.SetIdentityGroups(ctx, ib.ID, []string{"ops"}, at(11), at(101)))
	v1, err := s.AuthVersion(ctx)
	must(t, err)
	if v1 != v0+2 {
		t.Errorf("AuthVersion after two SetIdentityGroups = %d, want %d", v1, v0+2)
	}
	got, err = s.GetIdentity(ctx, "https://idp", "a")
	must(t, err)
	ia.LoginAt, ia.GroupsExpireAt = at(10), ptr(at(100))
	same(t, "the identity after its login", ia, *got)

	groups, err := s.ListIdentityGroups(ctx)
	must(t, err)
	ga := []index.IdentityGroup{
		{IdentityID: ia.ID, UserID: alice.ID, UID: alice.UID, Name: "team-a", ExpiresAt: ptr(at(100))},
		{IdentityID: ia.ID, UserID: alice.ID, UID: alice.UID, Name: "team-b", ExpiresAt: ptr(at(100))},
	}
	gb := index.IdentityGroup{IdentityID: ib.ID, UserID: bob.ID, UID: bob.UID, Name: "ops", ExpiresAt: ptr(at(101))}
	want := append(append([]index.IdentityGroup{}, ga...), gb)
	if alice.UID > bob.UID { // ordered by uid
		want = append([]index.IdentityGroup{gb}, ga...)
	}
	same(t, "ListIdentityGroups", want, groups)

	// A name twice, and a missing identity: nothing changes, auth_version neither.
	wantErr(t, "SetIdentityGroups with a name twice",
		s.SetIdentityGroups(ctx, ia.ID, []string{"x", "x"}, at(20), at(200)), index.ErrConflict)
	wantErr(t, "SetIdentityGroups of none", s.SetIdentityGroups(ctx, 9999, nil, at(20), at(200)), index.ErrNotFound)
	if v, err := s.AuthVersion(ctx); err != nil || v != v1 {
		t.Errorf("AuthVersion after refused SetIdentityGroups = %d, %v; want %d", v, err, v1)
	}
	groups, err = s.ListIdentityGroups(ctx)
	must(t, err)
	same(t, "ListIdentityGroups after refusals", want, groups)

	// The next login of alice has no groups: hers are gone, bob's stay.
	must(t, s.SetIdentityGroups(ctx, ia.ID, nil, at(30), at(300)))
	groups, err = s.ListIdentityGroups(ctx)
	must(t, err)
	same(t, "ListIdentityGroups after a login without groups", []index.IdentityGroup{gb}, groups)

	must(t, s.SetIdentityGroups(ctx, ib.ID, []string{"ops", "dev"}, at(40), at(400)))
	must(t, s.DeleteIdentity(ctx, ib.ID))
	wantErr(t, "DeleteIdentity twice", s.DeleteIdentity(ctx, ib.ID), index.ErrNotFound)
	groups, err = s.ListIdentityGroups(ctx)
	must(t, err)
	same(t, "ListIdentityGroups after DeleteIdentity", []index.IdentityGroup{}, groups)
	// The subject is free again.
	again := index.Identity{UserID: alice.ID, Issuer: "https://other", Subject: "a", CreatedAt: at(50)}
	must(t, s.CreateIdentity(ctx, &again))
}

// testOIDCLogins: the rows of oidc_logins, found by id, by the hash of a state or a device code
// and by a user code, each unique; "" finds nothing; a status changes from the one read only;
// expired rows are deleted, a device request with its code logins.
func testOIDCLogins(t *testing.T, s index.Store) {
	ctx := t.Context()
	u := index.User{Name: "carol", Kind: "oidc", CreatedAt: at(0)}
	must(t, s.CreateUser(ctx, &u))

	dev := index.OIDCLogin{Kind: index.OIDCLoginDevice, DeviceCode: "dc1", UserCode: "BCDFGHJK", Status: index.OIDCPending,
		CreatedAt: at(0), ExpiresAt: at(600)}
	must(t, s.CreateOIDCLogin(ctx, &dev))
	code := index.OIDCLogin{Kind: index.OIDCLoginCode, State: "st1", Binding: "b1", Nonce: "n1", Verifier: "v1",
		DeviceID: &dev.ID, Status: index.OIDCPending, CreatedAt: at(1), ExpiresAt: at(601)}
	must(t, s.CreateOIDCLogin(ctx, &code))
	plain := index.OIDCLogin{Kind: index.OIDCLoginCode, State: "st2", Binding: "b2", Nonce: "n2", Verifier: "v2",
		Status: index.OIDCPending, CreatedAt: at(2), ExpiresAt: at(2000)}
	must(t, s.CreateOIDCLogin(ctx, &plain))

	for _, tt := range []struct {
		name string
		get  func() (*index.OIDCLogin, error)
		want index.OIDCLogin
	}{
		{"GetOIDCLogin", func() (*index.OIDCLogin, error) { return s.GetOIDCLogin(ctx, code.ID) }, code},
		{"GetOIDCLoginByState", func() (*index.OIDCLogin, error) { return s.GetOIDCLoginByState(ctx, "st1") }, code},
		{"GetOIDCLoginByDeviceCode", func() (*index.OIDCLogin, error) { return s.GetOIDCLoginByDeviceCode(ctx, "dc1") }, dev},
		{"GetOIDCLoginByUserCode", func() (*index.OIDCLogin, error) { return s.GetOIDCLoginByUserCode(ctx, "BCDFGHJK") }, dev},
	} {
		got, err := tt.get()
		must(t, err)
		same(t, tt.name, tt.want, *got)
	}
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"GetOIDCLoginByState of none", second(s.GetOIDCLoginByState(ctx, "nope"))},
		{"GetOIDCLoginByState of \"\"", second(s.GetOIDCLoginByState(ctx, ""))},
		{"GetOIDCLoginByDeviceCode of \"\"", second(s.GetOIDCLoginByDeviceCode(ctx, ""))},
		{"GetOIDCLoginByUserCode of \"\"", second(s.GetOIDCLoginByUserCode(ctx, ""))},
		{"GetOIDCLogin of none", second(s.GetOIDCLogin(ctx, 9999))},
	} {
		wantErr(t, tt.name, tt.err, index.ErrNotFound)
	}
	for _, l := range []index.OIDCLogin{
		{Kind: index.OIDCLoginCode, State: "st1", Status: index.OIDCPending, ExpiresAt: at(600)},
		{Kind: index.OIDCLoginDevice, DeviceCode: "dc1", UserCode: "ZZZZZZZZ", Status: index.OIDCPending, ExpiresAt: at(600)},
		{Kind: index.OIDCLoginDevice, DeviceCode: "dc2", UserCode: "BCDFGHJK", Status: index.OIDCPending, ExpiresAt: at(600)},
	} {
		wantErr(t, "CreateOIDCLogin of a taken key", s.CreateOIDCLogin(ctx, &l), index.ErrConflict)
	}

	// The approval of the device: once, from pending.
	dev.Status, dev.UserID, dev.PolledAt = index.OIDCApproved, &u.ID, ptr(at(30))
	must(t, s.UpdateOIDCLogin(ctx, &dev, index.OIDCPending))
	again := dev
	again.Status = index.OIDCDenied
	wantErr(t, "UpdateOIDCLogin from a status it left", s.UpdateOIDCLogin(ctx, &again, index.OIDCPending), index.ErrConflict)
	got, err := s.GetOIDCLogin(ctx, dev.ID)
	must(t, err)
	same(t, "the device request approved", dev, *got)
	none := index.OIDCLogin{ID: 9999, Status: index.OIDCUsed}
	wantErr(t, "UpdateOIDCLogin of none", s.UpdateOIDCLogin(ctx, &none, index.OIDCPending), index.ErrNotFound)

	// Expiry: the device request goes, and the code login of it; the later one stays.
	n, err := s.ExpireOIDCLogins(ctx, at(601))
	must(t, err)
	if n < 1 {
		t.Errorf("ExpireOIDCLogins = %d, want the device request at least", n)
	}
	wantErr(t, "the expired device request", second(s.GetOIDCLogin(ctx, dev.ID)), index.ErrNotFound)
	wantErr(t, "the code login of the expired device request", second(s.GetOIDCLogin(ctx, code.ID)), index.ErrNotFound)
	if _, err := s.GetOIDCLogin(ctx, plain.ID); err != nil {
		t.Errorf("the login that has not expired: %v", err)
	}
}

// testOIDCLoginRace: callbacks of one state at once redeem it once.
func testOIDCLoginRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	l := index.OIDCLogin{Kind: index.OIDCLoginCode, State: "race", Status: index.OIDCPending, ExpiresAt: at(600)}
	must(t, s.CreateOIDCLogin(ctx, &l))
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			errs[i] = s.Tx(ctx, func(w index.Writer) error {
				row, err := w.GetOIDCLoginByState(ctx, "race")
				if err != nil {
					return err
				}
				row.Status = index.OIDCUsed
				return w.UpdateOIDCLogin(ctx, row, index.OIDCPending)
			})
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, index.ErrConflict):
			t.Errorf("a losing redemption: %v, want ErrConflict", err)
		}
	}
	if won != 1 {
		t.Errorf("%d redemptions succeeded, want 1", won)
	}
}
