package auth

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// refreshLogin makes alice's OIDC login with groups and returns her, her identity, the login's
// token and another token of hers.
func refreshLogin(t *testing.T, users *Users, st index.Store, groups []string) (*index.User, *index.Identity, *Token, *Token) {
	t.Helper()
	ctx := context.Background()
	u, err := users.LoginOIDC(ctx, OIDCLogin{Issuer: "https://idp", Subject: "s1", Username: "alice", Groups: groups,
		Lifetime: time.Hour}, Origin{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.GetIdentity(ctx, "https://idp", "s1")
	if err != nil {
		t.Fatal(err)
	}
	tokens := &Tokens{Index: st, Now: users.Now}
	login, _, err := tokens.LoginToken(ctx, u, time.Hour, LoginRefresh{}, Origin{})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := tokens.LoginToken(ctx, u, time.Hour, LoginRefresh{}, Origin{})
	if err != nil {
		t.Fatal(err)
	}
	return u, id, login, other
}

// checkLastAudit checks the newest audit row of action: its principal and subject, as want's
// principal and subject, and each other key of want in its details, compared as JSON.
func checkLastAudit(t *testing.T, st index.Reader, action string, want map[string]any) {
	t.Helper()
	evs, err := st.ListAudit(context.Background(), index.AuditFilter{Action: action, Limit: 1})
	if err != nil || len(evs) != 1 {
		t.Fatalf("%s: %+v, %v", action, evs, err)
	}
	got := map[string]any{}
	if err := json.Unmarshal(evs[0].Details, &got); err != nil {
		t.Fatal(err)
	}
	got["principal"], got["subject_"] = evs[0].Principal, evs[0].Subject
	for k, v := range want {
		if jsonText(got[k]) != jsonText(v) {
			t.Errorf("%s: %s = %s, want %s", action, k, jsonText(got[k]), jsonText(v))
		}
	}
}

// TestRevokeRefused: the token of a login the IdP refused is revoked once, with a token.revoke row
// by anonymous, source refresh, reason refused and refreshedAt; the user stays enabled and its
// other token live; a second refusal, as on another replica, writes nothing.
func TestRevokeRefused(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	u, _, login, other := refreshLogin(t, users, st, nil)
	tokens := &Tokens{Index: st, Now: users.Now}
	confirmed := usersNow.Add(-15 * time.Minute)
	revoke := func() *index.AuditEvent {
		t.Helper()
		var ev *index.AuditEvent
		if err := st.Tx(ctx, func(w index.Writer) (err error) {
			ev, err = tokens.RevokeRefused(ctx, w, login.ID, confirmed)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	if ev := revoke(); ev == nil {
		t.Fatal("a refusal wrote no row")
	}
	checkLastAudit(t, st, ActionTokenRevoke, map[string]any{"principal": Anonymous.String(), "subject_": UserPrincipal(u).String(),
		"source": SourceRefresh, "reason": ReasonRefused, "refreshedAt": confirmed.UTC().Format(time.RFC3339), "id": login.ID,
		"kind": TokenPAT, "name": login.Name})
	live, err := st.ListTokenPage(ctx, index.TokenFilter{Limit: 10, LiveAt: usersNow})
	if err != nil || len(live) != 1 || live[0].ID != other.ID {
		t.Errorf("the live tokens = %+v, %v; want the other one only", live, err)
	}
	if again, err := st.GetUserByID(ctx, u.ID); err != nil || again.Disabled {
		t.Errorf("the user after a refusal: %+v, %v", again, err)
	}
	if ev := revoke(); ev != nil {
		t.Errorf("a second refusal wrote %+v", ev)
	}
	if evs, _ := st.ListAudit(ctx, index.AuditFilter{Action: ActionTokenRevoke}); len(evs) != 1 {
		t.Errorf("token.revoke rows: %d, want 1", len(evs))
	}
}

// TestRefreshGroups: a refresh whose ID token has other groups replaces the identity's, with a
// user.groups row by anonymous, and leaves its login and the end of its groups as they were; the
// same groups in another order write nothing, nor move auth_version.
func TestRefreshGroups(t *testing.T) {
	ctx := context.Background()
	users, st, _ := newUsers(t)
	u, id, login, _ := refreshLogin(t, users, st, []string{"dev", "ops"})
	refresh := func(groups []string) *index.AuditEvent {
		t.Helper()
		var ev *index.AuditEvent
		if err := st.Tx(ctx, func(w index.Writer) (err error) {
			ev, err = users.RefreshGroups(ctx, w, "https://idp", "s1", groups, login.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	v0, _ := st.AuthVersion(ctx)
	if ev := refresh([]string{"ops", "dev"}); ev != nil {
		t.Errorf("the same groups wrote %+v", ev)
	}
	if v, _ := st.AuthVersion(ctx); v != v0 {
		t.Errorf("auth_version moved without a change: %d, %d", v0, v)
	}
	if ev := refresh([]string{"ops"}); ev == nil {
		t.Fatal("other groups wrote nothing")
	}
	checkLastAudit(t, st, ActionUserGroups, map[string]any{"principal": Anonymous.String(), "subject_": UserPrincipal(u).String(),
		"source": SourceRefresh, "groups": []string{"ops"}, "previous": []string{"dev", "ops"}, "token": login.ID,
		"issuer": "https://idp", "subject": "s1"})
	if after, err := st.GetIdentity(ctx, "https://idp", "s1"); err != nil || jsonText(after) != jsonText(id) {
		t.Errorf("the identity after a refresh: %+v, %v; want its login and end as %+v", after, err, id)
	}
	if groups, err := UserGroups(ctx, st, u, usersNow); err != nil || jsonText(groups) != `["ops"]` {
		t.Errorf("the groups after a refresh: %q, %v", groups, err)
	}
	if ev := refresh(nil); ev == nil {
		t.Error("no groups wrote nothing")
	}
	if groups, _ := UserGroups(ctx, st, u, usersNow); len(groups) != 0 {
		t.Errorf("the groups after a refresh without any: %q", groups)
	}
}
