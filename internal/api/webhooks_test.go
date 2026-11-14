package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// webhookGrants: carol (upstream_test.go) manages raw/open, alice reads it, bob has nothing.
var webhookGrants = map[string]bool{
	"user:admin manage */*":      true,
	"user:admin read */*":        true,
	"user:alice read raw/open":   true,
	"user:carol read raw/open":   true,
	"user:carol manage raw/open": true,
}

// webhookFixture is a fixture with the webhook service over its store and a master key, the
// repos raw/open and raw/secret, and webhooks that may reach loopback.
func webhookFixture(t *testing.T, master bool) *fixture {
	t.Helper()
	svc := &webhook.Service{Deliverer: webhook.NewDeliverer(webhook.Options{AllowedHosts: []string{"127.0.0.1"}}),
		Formats: oneOf("raw", "pypi"), Replica: "replica-test", Source: "https://art.example.com"}
	if master {
		m, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
		if err != nil {
			t.Fatal(err)
		}
		svc.Master = m
	}
	f := newFixture(t, Options{Auth: fakeAuth{grants: webhookGrants}, Webhooks: svc})
	svc.Index = f.st
	f.create(t, "raw", "open")
	f.create(t, "raw", "secret")
	return f
}

const hookBody = `{"url": "https://ci.example.com/hooks/tok", "events": ["version.published"]}`

// postWebhook creates a webhook at collection as p and returns the answer.
func (f *fixture) postWebhook(t *testing.T, collection, body string, p auth.Principal) CreatedWebhook {
	t.Helper()
	rr := f.do(http.MethodPost, collection, body, p)
	var c CreatedWebhook
	decodeJSON(t, rr, http.StatusCreated, &c)
	if loc, want := rr.Header().Get("Location"), fmt.Sprintf("%s/%d", collection, c.ID); loc != want {
		t.Errorf("Location %q, want %q", loc, want)
	}
	return c
}

// TestWebhookAuthorization: a repo's webhooks need manage on it, decided before the repo is
// looked up and before the body is read (401, 404, 403 as rbac.md §3); a webhook of another mask
// is 404 under a repo; /api/v1/webhooks is the admin's.
func TestWebhookAuthorization(t *testing.T) {
	f := webhookFixture(t, true)
	every := f.postWebhook(t, "/api/v1/webhooks", `{"mask": "*/*", "url": "https://audit.example.com/"}`, admin)
	own := f.postWebhook(t, "/api/v1/repos/raw/open/webhooks", hookBody, carol)
	for _, tt := range []struct {
		name           string
		p              auth.Principal
		method, target string
		body           string
		status         int
	}{
		{"anonymous lists", auth.Anonymous, "GET", "/api/v1/repos/raw/open/webhooks", "", 401},
		{"a stranger lists", bob, "GET", "/api/v1/repos/raw/open/webhooks", "", 404},
		{"a reader lists", alice, "GET", "/api/v1/repos/raw/open/webhooks", "", 403},
		{"the maintainer lists", carol, "GET", "/api/v1/repos/raw/open/webhooks", "", 200},
		{"the maintainer, another repo", carol, "GET", "/api/v1/repos/raw/secret/webhooks", "", 404},
		{"admin, a repo that does not exist", admin, "GET", "/api/v1/repos/raw/absent/webhooks", "", 404},
		{"a stranger's body that does not parse", bob, "POST", "/api/v1/repos/raw/open/webhooks", "{", 404},
		{"a reader's body that does not parse", alice, "POST", "/api/v1/repos/raw/open/webhooks", "{", 403},
		{"the maintainer's body that does not parse", carol, "POST", "/api/v1/repos/raw/open/webhooks", "{", 400},
		{"the maintainer, a webhook of another mask", carol, "GET", fmt.Sprintf("/api/v1/repos/raw/open/webhooks/%d", every.ID), "", 404},
		{"admin, a webhook of another mask under a repo", admin, "DELETE", fmt.Sprintf("/api/v1/repos/raw/open/webhooks/%d", every.ID), "", 404},
		{"a reader tests", alice, "POST", fmt.Sprintf("/api/v1/repos/raw/open/webhooks/%d/test", own.ID), "", 403},
		{"a reader rotates", alice, "POST", fmt.Sprintf("/api/v1/repos/raw/open/webhooks/%d/secret", own.ID), "", 403},
		{"a reader reads the log", alice, "GET", fmt.Sprintf("/api/v1/repos/raw/open/webhooks/%d/deliveries", own.ID), "", 403},
		{"anonymous, every mask", auth.Anonymous, "GET", "/api/v1/webhooks", "", 401},
		{"the maintainer, every mask", carol, "GET", "/api/v1/webhooks", "", 403},
		{"the maintainer creates a wildcard", carol, "POST", "/api/v1/webhooks", `{"mask": "raw/*", "url": "https://x.example.com/"}`, 403},
		{"the maintainer, her webhook by the admin's route", carol, "GET", fmt.Sprintf("/api/v1/webhooks/%d", own.ID), "", 403},
		{"admin, her webhook by the admin's route", admin, "GET", fmt.Sprintf("/api/v1/webhooks/%d", own.ID), "", 200},
		{"a mask under a repo's route", carol, "POST", "/api/v1/repos/raw/open/webhooks", `{"mask": "*/*", "url": "https://x.example.com/"}`, 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := f.do(tt.method, tt.target, tt.body, tt.p)
			wantStatus(t, rr, tt.status)
			wantChallenge(t, rr)
		})
	}

	// A credential that never reaches manage: a personal access token with scopes.
	scoped := auth.Credential{Kind: auth.CredPAT, TokenID: 1, Scopes: []string{"read", "write"}}
	wantStatus(t, f.doAs("GET", "/api/v1/repos/raw/open/webhooks", "", carol, scoped), 403)
	wantStatus(t, f.doAs("GET", "/api/v1/webhooks", "", admin, scoped), 403)

	var list WebhookList
	decodeJSON(t, f.do("GET", "/api/v1/repos/raw/open/webhooks", "", carol), 200, &list)
	if len(list.Webhooks) != 1 || list.Webhooks[0].ID != own.ID {
		t.Errorf("the repo's webhooks %+v, want its own only", list.Webhooks)
	}
	decodeJSON(t, f.do("GET", "/api/v1/webhooks", "", admin), 200, &list)
	if len(list.Webhooks) != 2 {
		t.Errorf("every webhook %+v, want 2", list.Webhooks)
	}
}

// TestWebhookLifecycle: create answers the secret once, a read never; a change of the URL, the
// events and active; a mask that cannot change and URLs the egress rules refuse are 400; a
// rotation answers a new secret; a delete takes the webhook. Each change is an audit row that
// names the URL's host, never the URL.
func TestWebhookLifecycle(t *testing.T) {
	f := webhookFixture(t, true)
	c := f.postWebhook(t, "/api/v1/repos/raw/open/webhooks", hookBody, carol)
	if _, err := webhook.ParseSecret(c.Secret); err != nil || c.Mask != "raw/open" || !c.Active || len(c.Events) != 1 {
		t.Fatalf("created %+v", c.Webhook)
	}
	path := fmt.Sprintf("/api/v1/repos/raw/open/webhooks/%d", c.ID)
	rr := f.do("GET", path, "", carol)
	wantStatus(t, rr, 200)
	if strings.Contains(rr.Body.String(), c.Secret) || strings.Contains(rr.Body.String(), "secret") {
		t.Errorf("a read shows the secret: %s", rr.Body.String())
	}
	var changed Webhook
	decodeJSON(t, f.do("PATCH", path, `{"events": [], "active": false, "url": "https://ci2.example.com/x"}`, carol), 200, &changed)
	if changed.Active || len(changed.Events) != 0 || changed.URL != "https://ci2.example.com/x" || changed.Mask != "raw/open" {
		t.Errorf("changed %+v", changed)
	}
	wantPatchRefused(t, f, path)
	var rotated Secret
	decodeJSON(t, f.do("POST", path+"/secret", "", carol), 200, &rotated)
	if rotated.Secret == c.Secret || !strings.HasPrefix(rotated.Secret, "whsec_") {
		t.Errorf("the rotated secret is not new")
	}
	wantStatus(t, f.do("DELETE", path, "", carol), 204)
	wantStatus(t, f.do("GET", path, "", carol), 404)
	wantWebhookAudit(t, f, c.ID)
}

// wantWebhookAudit checks the audit rows of the changes of the webhook id of raw/open: one of
// each action, naming the URL's host and never the URL's path or a secret.
func wantWebhookAudit(t *testing.T, f *fixture, id int64) {
	t.Helper()
	for _, action := range []string{webhook.ActionCreate, webhook.ActionUpdate, webhook.ActionSecret, webhook.ActionDelete} {
		ev := oneAudit(t, f.st, action)
		if ev.Repo != "raw/open" || ev.Subject != "webhook:"+strconv.FormatInt(id, 10) || ev.details["urlHost"] == nil ||
			strings.Contains(string(ev.Details), "/hooks/tok") || strings.Contains(string(ev.Details), "whsec_") {
			t.Errorf("%s: %+v %v", action, ev.AuditEvent, ev.details)
		}
	}
}

// wantPatchRefused checks the 400 of each change of the webhook at path that breaks a rule.
func wantPatchRefused(t *testing.T, f *fixture, path string) {
	t.Helper()
	for body, msg := range map[string]string{
		`{"mask": "raw/*"}`:                     "mask: cannot change",
		`{"url": "http://ci.example.com/"}`:     "url: must be https",
		`{"url": "https://169.254.169.254/x"}`:  "url: names an address that is not public",
		`{"url": "https://u:p@ci.example.com"}`: "url: must not hold user information",
		`{"events": ["push"]}`:                  `events: "push" is not one of`,
	} {
		wantError(t, f.do("PATCH", path, body, carol), 400, codeInvalid, msg)
	}
}

// TestWebhookTest: a test sends a signed webhook.ping now and answers its row of the delivery
// log; the log pages newest first.
func TestWebhookTest(t *testing.T) {
	f := webhookFixture(t, true)
	got := make(chan *http.Request, 4)
	bodies := make(chan []byte, 4)
	rec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r
		bodies <- b
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(rec.Close)
	c := f.postWebhook(t, "/api/v1/repos/raw/open/webhooks", fmt.Sprintf(`{"url": %q, "active": false}`, rec.URL), carol)
	path := fmt.Sprintf("/api/v1/repos/raw/open/webhooks/%d", c.ID)
	var d Delivery
	decodeJSON(t, f.do("POST", path+"/test", "", carol), 200, &d)
	if d.State != "delivered" || d.LastStatus != http.StatusAccepted || d.Attempts != 1 || d.EventType != webhook.TypePing {
		t.Fatalf("the test %+v, want delivered", d)
	}
	key, err := webhook.ParseSecret(c.Secret)
	if err != nil {
		t.Fatal(err)
	}
	r, body := <-got, <-bodies
	ts, _ := strconv.ParseInt(r.Header.Get("webhook-timestamp"), 10, 64)
	if r.Header.Get("webhook-signature") != webhook.Sign(key, d.EventID, ts, body) || !strings.Contains(string(body), `"webhook.test"`) {
		t.Errorf("the ping does not verify with the secret it was created with: %s", body)
	}
	decodeJSON(t, f.do("POST", path+"/test", "", carol), 200, &d)
	var page DeliveryList
	decodeJSON(t, f.do("GET", path+"/deliveries?limit=1", "", carol), 200, &page)
	if len(page.Deliveries) != 1 || page.Deliveries[0].ID != d.ID || page.Next == "" {
		t.Fatalf("the first page %+v", page)
	}
	var second DeliveryList
	decodeJSON(t, f.do("GET", path+"/deliveries?limit=1&cursor="+page.Next, "", carol), 200, &second)
	if len(second.Deliveries) != 1 || second.Deliveries[0].ID >= d.ID || second.Next != "" {
		t.Errorf("the second page %+v", second)
	}
	wantError(t, f.do("GET", path+"/deliveries?cursor=x", "", carol), 400, codeInvalid, "cursor")
}

// TestWebhookNoMasterKey: without a master key a webhook is neither created, rotated nor tested:
// 501 naming the key; it is still listed and deleted.
func TestWebhookNoMasterKey(t *testing.T) {
	f := webhookFixture(t, false)
	wantError(t, f.do("POST", "/api/v1/repos/raw/open/webhooks", hookBody, carol), 501, codeUnavailable, "keys.masterKeyEnv")
	wantStatus(t, f.do("GET", "/api/v1/repos/raw/open/webhooks", "", carol), 200)
}
