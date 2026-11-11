package storetest

import (
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testBootstrapToken: the one row of bootstrap_token. It is put when there is none or over the
// hash it replaces, never over another; it is used once, with its hash, before it expires; a
// replacement is unused; a use in a transaction that rolls back is no use.
func testBootstrapToken(t *testing.T, s index.Store) {
	ctx := t.Context()
	wantErr(t, "GetBootstrapToken of none", second(s.GetBootstrapToken(ctx)), index.ErrNotFound)
	wantErr(t, "UseBootstrapToken of none", s.UseBootstrapToken(ctx, "a", at(0)), index.ErrNotFound)

	first := index.BootstrapToken{Hash: "a", Replica: "r1:1", CreatedAt: at(0), ExpiresAt: at(3600)}
	must(t, s.PutBootstrapToken(ctx, &first, ""))
	got, err := s.GetBootstrapToken(ctx)
	must(t, err)
	same(t, "GetBootstrapToken", first, *got)
	for _, old := range []string{"", "b"} {
		other := index.BootstrapToken{Hash: "c", Replica: "r2:1", ExpiresAt: at(3600)}
		wantErr(t, "PutBootstrapToken over "+strconv.Quote(old), s.PutBootstrapToken(ctx, &other, old), index.ErrConflict)
	}

	errBoom := errors.New("boom")
	err = s.Tx(ctx, func(w index.Writer) error {
		must(t, w.UseBootstrapToken(ctx, "a", at(10)))
		return errBoom
	})
	wantErr(t, "Tx", err, errBoom)
	for _, tt := range []struct {
		name string
		hash string
		at   int
	}{
		{"another hash", "b", 10},
		{"at the expiry", "a", 3600},
		{"after the expiry", "a", 3601},
	} {
		wantErr(t, "UseBootstrapToken of "+tt.name, s.UseBootstrapToken(ctx, tt.hash, at(tt.at)), index.ErrNotFound)
	}
	must(t, s.UseBootstrapToken(ctx, "a", at(20)))
	wantErr(t, "UseBootstrapToken twice", s.UseBootstrapToken(ctx, "a", at(21)), index.ErrNotFound)
	got, err = s.GetBootstrapToken(ctx)
	must(t, err)
	first.UsedAt = ptr(at(20))
	same(t, "the token used", first, *got)

	// A replacement of the used token: a new one, valid until its own expiry.
	next := index.BootstrapToken{Hash: "d", Replica: "r2:1", CreatedAt: at(4000), ExpiresAt: at(7600)}
	must(t, s.PutBootstrapToken(ctx, &next, "a"))
	got, err = s.GetBootstrapToken(ctx)
	must(t, err)
	same(t, "the token replaced", next, *got)
	wantErr(t, "UseBootstrapToken of the replaced hash", s.UseBootstrapToken(ctx, "a", at(4001)), index.ErrNotFound)
	must(t, s.UseBootstrapToken(ctx, "d", at(4001)))
}

// testBootstrapTokenRace: replicas that make the token at once leave one, theirs whose put
// succeeded; exchanges of it at once, each in a transaction that also creates a user, commit
// once.
func testBootstrapTokenRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	const replicas = 8
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Go(func() {
			tok := index.BootstrapToken{Hash: "h" + strconv.Itoa(i), Replica: "r" + strconv.Itoa(i), ExpiresAt: at(3600)}
			errs[i] = s.PutBootstrapToken(ctx, &tok, "")
		})
	}
	wg.Wait()
	stored, err := s.GetBootstrapToken(ctx)
	must(t, err)
	winners := 0
	for i, err := range errs {
		if err != nil {
			wantErr(t, "PutBootstrapToken of replica "+strconv.Itoa(i), err, index.ErrConflict)
			continue
		}
		winners++
		if stored.Hash != "h"+strconv.Itoa(i) {
			t.Errorf("replica %d made the token, the stored one is %s's", i, stored.Replica)
		}
	}
	if winners != 1 {
		t.Fatalf("%d replicas made the token, want 1", winners)
	}

	for i := range replicas {
		wg.Go(func() {
			errs[i] = s.Tx(ctx, func(w index.Writer) error {
				if err := w.UseBootstrapToken(ctx, stored.Hash, at(10)); err != nil {
					return err
				}
				return w.CreateUser(ctx, &index.User{Name: "admin" + strconv.Itoa(i), Kind: "local"})
			})
		})
	}
	wg.Wait()
	used := 0
	for i, err := range errs {
		if err != nil {
			wantErr(t, "exchange "+strconv.Itoa(i), err, index.ErrNotFound)
			continue
		}
		used++
	}
	users, err := s.ListUsers(ctx)
	must(t, err)
	if used != 1 || len(users) != 1 {
		t.Errorf("%d exchanges committed and %d users exist, want 1 and 1", used, len(users))
	}
}
