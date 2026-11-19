package auth

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestRefreshDue: a refresh is due the interval after its base, less up to a tenth of it, never
// later; the times spread.
func TestRefreshDue(t *testing.T) {
	base, d := usersNow, 15*time.Minute
	seen := map[time.Time]bool{}
	for range 200 {
		due := RefreshDue(base, d)
		if due.Before(base.Add(d-d/10)) || due.After(base.Add(d)) {
			t.Fatalf("RefreshDue = %v, want within [%v, %v]", due, base.Add(d-d/10), base.Add(d))
		}
		seen[due] = true
	}
	if len(seen) < 100 {
		t.Errorf("RefreshDue spread over %d times of 200", len(seen))
	}
}

// TestLoginOIDCRefresh: with auth.oidc.refresh.interval the login keeps its refresh token in its
// transaction, with the identity's id, and user.login says whether it did; without the setting
// user.login has no refresh; a keep that fails rolls the login back.
func TestLoginOIDCRefresh(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	base := OIDCLogin{Issuer: "https://idp", Subject: "s1", Username: "olga", Groups: []string{"dev"}, Lifetime: time.Hour}
	var keptFor int64
	for _, tt := range []struct {
		name string
		keep func(context.Context, index.Writer, int64) (bool, error)
		want string // the refresh of user.login, "" for none
	}{
		{"off", nil, ""},
		{"kept", func(_ context.Context, _ index.Writer, id int64) (bool, error) { keptFor = id; return true, nil }, "true"},
		{"no refresh token", func(context.Context, index.Writer, int64) (bool, error) { return false, nil }, "false"},
	} {
		l := base
		l.Refresh = tt.keep
		if _, err := users.LoginOIDC(ctx, l, Origin{}); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		evs, err := st.ListAudit(ctx, index.AuditFilter{Action: ActionUserLogin, Limit: 1})
		if err != nil || len(evs) != 1 {
			t.Fatalf("%s: user.login = %+v, %v", tt.name, evs, err)
		}
		var details map[string]any
		_ = json.Unmarshal(evs[0].Details, &details)
		if got, ok := details["refresh"]; (tt.want == "" && ok) || (tt.want != "" && jsonText(got) != tt.want) {
			t.Errorf("%s: user.login's refresh = %v (%v), want %q", tt.name, got, ok, tt.want)
		}
	}
	id, err := st.GetIdentity(ctx, "https://idp", "s1")
	if err != nil || id.ID != keptFor {
		t.Errorf("the identity kept for = %d, want %+v, %v", keptFor, id, err)
	}
	errBoom := errors.New("boom")
	l := base
	l.Subject, l.Username = "s2", "oscar"
	l.Refresh = func(context.Context, index.Writer, int64) (bool, error) { return false, errBoom }
	if _, err := users.LoginOIDC(ctx, l, Origin{}); !errors.Is(err, errBoom) {
		t.Errorf("a login whose keep fails = %v", err)
	}
	if _, err := st.GetUser(ctx, "oscar"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the user of a login rolled back: %v", err)
	}
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestLoginTokenJoin: the mint of a login's token joins the row of its login to the token, due
// the interval after the mint less the jitter; a login without a row mints all the same, and a
// zero interval joins nothing.
func TestLoginTokenJoin(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	u, err := users.LoginOIDC(ctx, OIDCLogin{Issuer: "https://idp", Subject: "s1", Username: "olga", Lifetime: time.Hour}, Origin{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.GetIdentity(ctx, "https://idp", "s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, login := range []int64{41, 42} {
		row := index.OIDCRefresh{LoginID: &login, IdentityID: id.ID, Sealed: []byte("sealed"), NextAt: usersNow.Add(10 * time.Minute)}
		if err := st.CreateOIDCRefresh(ctx, &row); err != nil {
			t.Fatal(err)
		}
	}
	tokens := &Tokens{Index: st, Now: users.Now}
	interval := 15 * time.Minute
	joined, _, err := tokens.LoginToken(ctx, u, 2*time.Hour, LoginRefresh{Login: 41, Interval: interval}, Origin{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tokens.LoginToken(ctx, u, 2*time.Hour, LoginRefresh{Login: 99, Interval: interval}, Origin{}); err != nil {
		t.Errorf("the mint of a login without a refresh token: %v", err)
	}
	if _, _, err := tokens.LoginToken(ctx, u, 2*time.Hour, LoginRefresh{Login: 42}, Origin{}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ClaimOIDCRefresh(ctx, "r", usersNow.Add(time.Hour), usersNow.Add(2*time.Hour), 10)
	if err != nil || len(rows) != 2 || rows[0].TokenID != nil {
		t.Fatalf("the rows = %+v, %v; want two, the one of the zero interval not joined", rows, err)
	}
	if r := rows[1]; !joinedTo(r, joined.ID, usersNow, interval) {
		t.Errorf("the row after the mint: %+v; want joined to token %d", r, joined.ID)
	}
}

// joinedTo reports whether r is joined to the token tokenID minted at mint, due interval after it
// less the jitter.
func joinedTo(r index.OIDCRefresh, tokenID int64, mint time.Time, interval time.Duration) bool {
	return r.TokenID != nil && *r.TokenID == tokenID && r.LoginID == nil && !r.NextAt.Before(mint.Add(interval-interval/10)) &&
		!r.NextAt.After(mint.Add(interval))
}
