package storetest

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// accessSummary is what a test compares of an index.Access: each user as a line of what the
// report reads of it, and the ids of the rows of the other lists.
type accessSummary struct {
	Users          []string
	IdentityGroups []index.IdentityGroup
	Bindings       []index.Binding
	Tokens         []int64
	TokenRepos     []int64
	AnonymousRepos []int64
	CIPolicies     []string
}

// summarize returns the accessSummary of a.
func summarize(a *index.Access) accessSummary {
	sum := accessSummary{IdentityGroups: a.IdentityGroups, Bindings: a.Bindings, Tokens: []int64{}, TokenRepos: repoIDs(a.TokenRepos),
		AnonymousRepos: repoIDs(a.AnonymousRepos), CIPolicies: []string{}, Users: []string{}}
	for _, u := range a.Users {
		seen := "never"
		if u.LastSeenAt != nil {
			seen = u.LastSeenAt.Format(time.RFC3339)
		}
		sum.Users = append(sum.Users, fmt.Sprintf("%s %s disabled=%t owner=%s seen=%s hash=%q history=%d", u.Name, u.Kind, u.Disabled,
			u.Owner, seen, u.PasswordHash, len(u.PasswordHistory)))
	}
	for _, tok := range a.Tokens {
		sum.Tokens = append(sum.Tokens, tok.ID)
	}
	for _, p := range a.CIPolicies {
		sum.CIPolicies = append(sum.CIPolicies, p.Name+" "+p.State)
	}
	return sum
}

// accessFixture writes into s the rows of every kind that ReadAccess reads or leaves out, and
// returns the summary of what it must read: users live, disabled, deleted, a service account
// and an OIDC user; groups current, lapsed and of a deleted user; bindings; tokens live,
// expired, revoked and without an expiry, of users, of repos and over a mask; and CI trust
// policies expired and active.
func accessFixture(t *testing.T, s index.Store) accessSummary {
	t.Helper()
	ctx := t.Context()
	future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	alice := newUser(t, s, "alice")
	alice.PasswordHistory = []string{passwordHash("before")}
	must(t, s.UpdateUser(ctx, &alice))
	off := newUser(t, s, "off")
	off.Disabled = true
	must(t, s.UpdateUser(ctx, &off))
	gone := newUser(t, s, "gone")
	gone.DeletedAt, gone.DeletedName, gone.Name, gone.Disabled = ptr(at(5)), "gone", "~"+gone.UID, true
	must(t, s.UpdateUser(ctx, &gone))
	bot := index.User{Name: "bot", Kind: "service", Owner: "user:" + alice.UID}
	must(t, s.CreateUser(ctx, &bot))
	sso := index.User{Name: "sso", Kind: "oidc"}
	must(t, s.CreateUser(ctx, &sso))
	must(t, s.TouchUser(ctx, sso.ID, at(50)))

	ia := index.Identity{UserID: sso.ID, Issuer: "https://idp", Subject: "sso"}
	must(t, s.CreateIdentity(ctx, &ia))
	must(t, s.SetIdentityGroups(ctx, ia.ID, []string{"ops", "dev"}, at(10), future))
	ib := index.Identity{UserID: sso.ID, Issuer: "https://other", Subject: "sso"}
	must(t, s.CreateIdentity(ctx, &ib))
	must(t, s.SetIdentityGroups(ctx, ib.ID, []string{"lapsed"}, at(10), past))
	ig := index.Identity{UserID: gone.ID, Issuer: "https://idp", Subject: "gone"}
	must(t, s.CreateIdentity(ctx, &ig))
	must(t, s.SetIdentityGroups(ctx, ig.ID, []string{"ops"}, at(10), future))

	raw := newRepo(t, s, "raw", "app")
	public := newRepo(t, s, "raw", "public")
	public.AnonymousRead = true
	must(t, s.UpdateRepo(ctx, &public))
	newRepo(t, s, "pypi", "quiet")

	var bindings []index.Binding
	for _, b := range [][3]string{{"user:" + alice.UID, "admin", "*/*"}, {"group:ops", "writer", "raw/*"},
		{"cert:spiffe://corp/ci/*", "reader", "raw/app"}, {"user:" + bot.UID, "writer", "raw/app"}} {
		row := index.Binding{Principal: b[0], Role: b[1], RepoMask: b[2], CreatedAt: at(20)}
		must(t, s.CreateBinding(ctx, &row))
		bindings = append(bindings, row)
	}

	token := func(tok index.Token) int64 {
		t.Helper()
		tok.Hash = fmt.Sprintf("%064d", len(tok.Name)) + tok.Name
		tok.CreatedAt = at(30)
		must(t, s.CreateToken(ctx, &tok))
		return tok.ID
	}
	live := token(index.Token{Kind: "pat", UserID: &alice.ID, Name: "live", ExpiresAt: ptr(future)})
	token(index.Token{Kind: "pat", UserID: &alice.ID, Name: "expired", ExpiresAt: ptr(past)})
	must(t, s.RevokeToken(ctx, token(index.Token{Kind: "pat", UserID: &bot.ID, Name: "revoked"}), at(40)))
	forever := token(index.Token{Kind: "pat", UserID: &bot.ID, Name: "forever", Scopes: []string{"read"}})
	deploy := token(index.Token{Kind: "deploy", RepoID: &raw.ID, Name: "ci", Scopes: []string{"read"}})
	mask := token(index.Token{Kind: "deploy", RepoMask: "*/*", Name: "fleet", Scopes: []string{"read"}, ExpiresAt: ptr(future)})
	token(index.Token{Kind: "deploy", RepoID: &public.ID, Name: "old", ExpiresAt: ptr(past)})

	expired := newCIPolicy(t, s, "expired")
	expired.State = index.CIPolicyExpired
	must(t, s.UpdateCIPolicy(ctx, &expired))
	reads := newCIPolicy(t, s, "reads")

	return accessSummary{
		Users: []string{
			`alice local disabled=false owner= seen=never hash="" history=0`,
			`bot service disabled=false owner=user:` + alice.UID + ` seen=never hash="" history=0`,
			`off local disabled=true owner= seen=never hash="" history=0`,
			`sso oidc disabled=false owner= seen=` + at(50).Format(time.RFC3339) + ` hash="" history=0`,
		},
		IdentityGroups: []index.IdentityGroup{
			{IdentityID: ia.ID, UserID: sso.ID, UID: sso.UID, Name: "dev", ExpiresAt: ptr(normalized(future))},
			{IdentityID: ia.ID, UserID: sso.ID, UID: sso.UID, Name: "ops", ExpiresAt: ptr(normalized(future))},
		},
		Bindings:       bindings,
		Tokens:         []int64{live, forever, deploy, mask},
		TokenRepos:     []int64{raw.ID},
		AnonymousRepos: []int64{public.ID},
		CIPolicies:     []string{"expired expired", "reads " + reads.State},
	}
}

// testAccess: ReadAccess reads who has what at the database's time (docs/spec/index-schema.md,
// The access report; #1279): the live users, a disabled one too, without their password hashes;
// the groups of their identities that have not lapsed; every binding; the live tokens, the
// deploy tokens over a mask among them, and the repos of the deploy tokens of one repo; every CI
// trust policy of the API, an expired one too; and the repos with anonymous_read (accessFixture).
// An empty index has every list empty, not nil. It writes nothing, and inside Tx it reads the
// transaction's own rows.
func testAccess(t *testing.T, s index.Store) {
	ctx := t.Context()
	empty, err := s.ReadAccess(ctx)
	must(t, err)
	same(t, "ReadAccess of an empty index", accessSummary{Users: []string{}, IdentityGroups: []index.IdentityGroup{},
		Bindings: []index.Binding{}, Tokens: []int64{}, TokenRepos: []int64{}, AnonymousRepos: []int64{}, CIPolicies: []string{}},
		summarize(empty))

	want := accessFixture(t, s)
	audit := auditCount(t, s)
	before := time.Now().Add(-time.Second)
	got, err := s.ReadAccess(ctx)
	must(t, err)
	if got.ReadAt.Before(before) || got.ReadAt.After(time.Now().Add(time.Second)) || got.ReadAt.Location() != time.UTC {
		t.Errorf("ReadAt %v, want the database's time now, in UTC", got.ReadAt)
	}
	if n := auditCount(t, s); n != audit {
		t.Errorf("ReadAccess wrote %d audit rows", n-audit)
	}
	same(t, "ReadAccess", want, summarize(got))

	must(t, s.Tx(ctx, func(w index.Writer) error {
		newUser(t, w, "carol")
		in, err := w.ReadAccess(ctx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(in.Users, func(u index.User) bool { return u.Name == "carol" }) {
			t.Errorf("ReadAccess in a transaction: no carol, the transaction's own user")
		}
		return nil
	}))
}

// normalized is t as the index keeps it, to the microsecond in UTC.
func normalized(t time.Time) time.Time { return t.Truncate(time.Microsecond).UTC() }

// repoIDs returns the ids of repos, in their order.
func repoIDs(repos []index.Repo) []int64 {
	ids := []int64{}
	for _, r := range repos {
		ids = append(ids, r.ID)
	}
	return ids
}

// auditCount returns how many audit rows s has.
func auditCount(t *testing.T, s index.Reader) int {
	t.Helper()
	rows, err := s.ListAudit(t.Context(), index.AuditFilter{Feed: true, Limit: 1 << 20})
	must(t, err)
	return len(rows)
}

// testAccessConsistent: the access report is one read. While a writer makes repos with a deploy
// token each and deletes the repo of two rounds before, the token's row going with it, every
// read has the repo of every deploy token of one repo it has: a read of tokens later than the
// read of repos would find the token of a repo made in between.
func testAccessConsistent(t *testing.T, s index.Store) {
	ctx := t.Context()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		var made []int64
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			r := index.Repo{Format: "raw", Name: fmt.Sprintf("r%d", i), Registry: "local", Prefix: fmt.Sprintf("raw/r%d", i),
				Kind: index.KindHosted}
			if s.CreateRepo(ctx, &r) != nil {
				continue
			}
			_ = s.CreateToken(ctx, &index.Token{Kind: "deploy", RepoID: &r.ID, Name: "ci", Hash: "h" + r.Name})
			if made = append(made, r.ID); len(made) > 2 {
				_ = s.DeleteRepo(ctx, made[0])
				made = made[1:]
			}
		}
	})
	defer wg.Wait()
	defer close(stop)
	for range 300 {
		a, err := s.ReadAccess(ctx)
		must(t, err)
		repos := map[int64]bool{}
		for _, r := range a.TokenRepos {
			repos[r.ID] = true
		}
		for _, tok := range a.Tokens {
			if tok.RepoID != nil && !repos[*tok.RepoID] {
				t.Fatalf("a read has the token %d of the repo %d and not the repo", tok.ID, *tok.RepoID)
			}
		}
	}
}
