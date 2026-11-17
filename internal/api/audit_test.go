package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// appendAudit writes the events of actions at at, one second apart, by principal on repo.
func appendAudit(t *testing.T, st index.Store, at time.Time, principal, repo string, actions ...string) {
	t.Helper()
	for i, a := range actions {
		ev := index.AuditEvent{At: at.Add(time.Duration(i) * time.Second), Principal: principal, Action: a, Repo: repo, IP: "192.0.2.1"}
		if err := st.AppendAudit(context.Background(), &ev); err != nil {
			t.Fatal(err)
		}
	}
}

// actionsOf pages through target, a page of limit at a time, and returns the actions in the
// order of the pages; between the first and the second page it calls between, if not nil.
func (f *fixture) actionsOf(t *testing.T, target string, limit int, between func()) []string {
	t.Helper()
	actions := []string{}
	next := fmt.Sprintf("%s&limit=%d", target, limit)
	for pages := 0; next != ""; pages++ {
		var list AuditList
		decodeJSON(t, f.do(http.MethodGet, next, "", admin), http.StatusOK, &list)
		for _, ev := range list.Events {
			actions = append(actions, ev.Action)
		}
		next = ""
		if list.Next != "" {
			next = fmt.Sprintf("%s&limit=%d&cursor=%s", target, limit, list.Next)
		}
		if pages == 0 && between != nil {
			between()
		}
	}
	return actions
}

// TestAuditAPI reads the audit log newest first, by filter, a page at a time, with a cursor
// that events written between two pages do not shift; a user is found by its name or uid.
func TestAuditAPI(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "alice")
	alice := "user:" + f.uidOf(t, "alice")
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	appendAudit(t, f.st, t0, alice, "raw/a", "push", "push", "delete")
	appendAudit(t, f.st, t0.Add(time.Minute), "token:3", "raw/b", "push")
	same := t0.Add(2 * time.Minute) // three events at one time: the cursor tells them apart by id
	for _, a := range []string{"e1", "e2", "e3"} {
		appendAudit(t, f.st, same, alice, "raw/a", a)
	}

	between := func() { appendAudit(t, f.st, t0.Add(time.Hour), alice, "raw/a", "later") }
	got := f.actionsOf(t, "/api/v1/audit?repo=raw/a", 2, between)
	wantJSON(t, "raw/a in pages of 2", got, `["e3", "e2", "e1", "delete", "push", "push"]`)
	for _, q := range []string{"user:alice", alice} {
		got := f.actionsOf(t, "/api/v1/audit?principal="+url.QueryEscape(q)+"&action=push", 10, nil)
		wantJSON(t, "alice's pushes by "+q, got, `["push", "push"]`)
	}
	since, until := url.QueryEscape(t0.Add(time.Second).Format(time.RFC3339)), url.QueryEscape(same.Format(time.RFC3339))
	got = f.actionsOf(t, "/api/v1/audit?since="+since+"&until="+until, 10, nil)
	wantJSON(t, "since 12:00:01, until 12:02", got, `["push", "delete", "push"]`)
	got = f.actionsOf(t, "/api/v1/audit?principal=user:dave", 10, nil)
	wantJSON(t, "a user that does not exist", got, `[]`)

	var list AuditList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/audit?principal=token:3", "", admin), http.StatusOK, &list)
	wantJSON(t, "the event of token 3", list.Events, fmt.Sprintf(`[{"id": %d, "at": "2026-11-09T12:01:00Z", "principal": "token:3",
		"action": "push", "repo": "raw/b", "ip": "192.0.2.1", "state": "ok"}]`, list.Events[0].ID))

	// The rows of registry writes that did not end ok, by state (#473).
	for _, ev := range []index.AuditEvent{
		{At: t0, Principal: "token:3", Action: "delete", Repo: "raw/b", State: index.AuditFailed, Error: "ingest: index rows: boom"},
		{At: t0, Principal: "token:3", Action: "push", Repo: "raw/b", State: index.AuditUnknown},
	} {
		if err := f.st.AppendAudit(context.Background(), &ev); err != nil {
			t.Fatal(err)
		}
	}
	var failed AuditList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/audit?state=failed", "", admin), http.StatusOK, &failed)
	wantJSON(t, "the failed row", failed.Events, fmt.Sprintf(`[{"id": %d, "at": "2026-11-09T12:00:00Z", "principal": "token:3",
		"action": "delete", "repo": "raw/b", "state": "failed", "error": "ingest: index rows: boom"}]`, failed.Events[0].ID))
	got = f.actionsOf(t, "/api/v1/audit?state=unknown", 10, nil)
	wantJSON(t, "the unknown row", got, `["push"]`)
}

// TestAuditAPIRefused checks the answers to a query the audit API refuses, and who may read it.
func TestAuditAPIRefused(t *testing.T) {
	f := newFixture(t, Options{})
	for _, tt := range []struct{ target, msg string }{
		{"/api/v1/audit?since=yesterday", "since"},
		{"/api/v1/audit?until=2026-11-09", "until"},
		{"/api/v1/audit?principal=alice", "principal"},
		{"/api/v1/audit?state=done", "state"},
		{"/api/v1/audit?cursor=eA", "cursor"},
		{"/api/v1/audit?limit=-1", "limit"},
		{"/api/v1/audit?after=-1", "after"},
		{"/api/v1/audit?after=x", "after"},
		{"/api/v1/audit?after=1.5", "after"},
		{"/api/v1/audit?after=0&limit=1001", "limit"},
		{"/api/v1/audit?after=0&cursor=eA", "after: the feed has no cursor"},
		{"/api/v1/audit?after=0&since=yesterday", "since"},
	} {
		t.Run(tt.target, func(t *testing.T) {
			wantError(t, f.do(http.MethodGet, tt.target, "", admin), http.StatusBadRequest, codeInvalid, tt.msg)
		})
	}
	scoped := auth.Credential{Kind: auth.CredPAT, TokenID: 1, UserID: 1, Scopes: []string{"read"}}
	for _, tt := range []struct {
		name   string
		authz  bool
		p      auth.Principal
		c      auth.Credential
		status int
	}{
		{"admin", true, admin, auth.Credential{Kind: auth.CredPassword}, 200},
		{"anonymous", true, auth.Anonymous, auth.Credential{}, 401},
		{"a user", true, alice, auth.Credential{Kind: auth.CredPassword}, 403},
		{"a scoped pat", false, admin, scoped, 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := Options{}
			if tt.authz {
				o.Auth = fakeAuth{grants: grants}
			}
			f := newFixture(t, o)
			f.addUsers(t, "admin", "alice")
			wantStatus(t, f.doAs(http.MethodGet, "/api/v1/audit", "", tt.p, tt.c), tt.status)
		})
	}
}

// feedOf reads the feed at target and returns its events' "<id> <action>", checking that the
// page has no next.
func (f *fixture) feedOf(t *testing.T, target string) []string {
	t.Helper()
	var list AuditList
	decodeJSON(t, f.do(http.MethodGet, target, "", admin), http.StatusOK, &list)
	if list.Next != "" {
		t.Errorf("%s: next %q, want none in the feed", target, list.Next)
	}
	out := []string{}
	for _, ev := range list.Events {
		out = append(out, fmt.Sprintf("%d %s", ev.ID, ev.Action))
	}
	return out
}

// TestAuditFeed reads the feed (#1278): the events above an id by id ascending whatever their
// times, with the list's filters and limit, no next, an empty page at the end; the events as the
// list answers them.
func TestAuditFeed(t *testing.T) {
	f := newFixture(t, Options{})
	f.addUsers(t, "alice")
	alice := "user:" + f.uidOf(t, "alice")
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	appendAudit(t, f.st, t0.Add(time.Hour), alice, "raw/a", "push")
	appendAudit(t, f.st, t0, "token:3", "raw/b", "push")
	appendAudit(t, f.st, t0.Add(30*time.Minute), alice, "raw/a", "delete")
	appendAudit(t, f.st, t0.Add(2*time.Hour), alice, "raw/b", "repo.create")
	rows, err := f.st.ListAudit(t.Context(), index.AuditFilter{Feed: true})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, r := range rows {
		ids[r.Action+" "+r.Repo] = r.ID
	}
	ev := func(keys ...string) []string {
		out := []string{}
		for _, k := range keys {
			out = append(out, fmt.Sprintf("%d %s", ids[k], strings.Fields(k)[0]))
		}
		return out
	}
	push, other, del, create := "push raw/a", "push raw/b", "delete raw/a", "repo.create raw/b"
	for _, tt := range []struct {
		query string
		want  []string
	}{
		{"after=0", ev(push, other, del, create)},
		{fmt.Sprintf("after=%d", ids[push]), ev(other, del, create)},
		{fmt.Sprintf("after=%d", ids[create]), ev()},
		{"after=0&limit=2", ev(push, other)},
		{fmt.Sprintf("after=%d&limit=1", ids[other]), ev(del)},
		{"after=0&repo=raw/a", ev(push, del)},
		{"after=0&principal=user:alice&action=push", ev(push)},
		{"after=0&principal=" + url.QueryEscape(alice), ev(push, del, create)},
		{"after=0&state=ok&since=" + url.QueryEscape(t0.Add(time.Minute).Format(time.RFC3339)) + "&until=" +
			url.QueryEscape(t0.Add(2*time.Hour).Format(time.RFC3339)), ev(push, del)},
		{"after=0&principal=user:dave", ev()},
	} {
		if got := f.feedOf(t, "/api/v1/audit?"+tt.query); !slices.Equal(got, tt.want) {
			t.Errorf("%s: %q, want %q", tt.query, got, tt.want)
		}
	}
	var feed, list AuditList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/audit?after=0", "", admin), http.StatusOK, &feed)
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/audit", "", admin), http.StatusOK, &list)
	slices.SortFunc(list.Events, func(a, b AuditEntry) int { return cmp.Compare(a.ID, b.ID) })
	want, err := json.Marshal(list.Events)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON(t, "the feed's events, the list's by id", feed.Events, string(want))
}
