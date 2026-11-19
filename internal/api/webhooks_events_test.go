package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// TestRepoWebhookEvents: a repo created and deleted writes repo.created and repo.deleted in the
// transaction of its audit row, with the row's id; a webhook of another mask gets neither.
func TestRepoWebhookEvents(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})
	f.repos.Webhooks = &webhook.Outbox{Source: "https://art.example.com"}
	all := index.Webhook{Mask: "raw/*", URL: "https://hooks.example.com/", SecretEnc: []byte("sealed"), Active: true}
	other := index.Webhook{Mask: "pypi/*", URL: "https://hooks.example.com/", SecretEnc: []byte("sealed"), Active: true}
	for _, h := range []*index.Webhook{&all, &other} {
		if err := f.st.CreateWebhook(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	f.create(t, "raw", "scratch")
	if err := f.repos.Delete(ctx, "raw", "scratch", false, Origin{Source: SourceAPI, RequestID: "req-9"}); err != nil {
		t.Fatal(err)
	}
	ds, err := f.st.ListWebhookDeliveries(ctx, all.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds[1].EventType != webhook.TypeRepoCreated || ds[0].EventType != webhook.TypeRepoDeleted {
		t.Fatalf("deliveries %+v, want repo.created then repo.deleted", ds)
	}
	var doc struct {
		Source string         `json:"source"`
		Data   map[string]any `json:"data"`
	}
	if err := json.Unmarshal(ds[0].Payload, &doc); err != nil {
		t.Fatal(err)
	}
	deleted := auditOf(t, f.st, "repo.delete")[0]
	repo, _ := doc.Data["repo"].(map[string]any)
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"source", doc.Source, "https://art.example.com/raw/scratch"},
		{"action", doc.Data["action"], "repo.delete"},
		{"audit", doc.Data["audit"], float64(deleted.ID)},
		{"requestId", doc.Data["requestId"], "req-9"},
		{"versions", doc.Data["versions"], 0.0},
		{"force", doc.Data["force"], false},
		{"repo.name", repo["name"], "scratch"},
		{"repo.kind", repo["kind"], "hosted"},
	} {
		if c.got != c.want {
			t.Errorf("repo.deleted %s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if ds, _ := f.st.ListWebhookDeliveries(ctx, other.ID, 0, 10); len(ds) != 0 {
		t.Errorf("a webhook of pypi/* got %d events of raw/scratch", len(ds))
	}
}

// TestApprovalWebhookEvents: under auth.approval second-admin, the requests made through the API,
// their approval and their withdrawal write approval.* to the webhooks of the admin that cover the
// change's mask, each in the transaction of its audit row; a repo's webhook gets none, and no
// event holds the secret of a deploy token nor its hash (docs/spec/rbac.md §13, #1336).
func TestApprovalWebhookEvents(t *testing.T) {
	ctx := context.Background()
	f := approvalFixture(t)
	f.repos.Webhooks = &webhook.Outbox{Source: "https://art.example.com"}
	f.secondAdmin(true)
	hooks := map[string]*index.Webhook{}
	for _, mask := range []string{"*/*", "raw/*", "raw/open"} {
		h := &index.Webhook{Mask: mask, URL: "https://hooks.example.com/", SecretEnc: []byte("sealed"), Active: true}
		must(t, f.st.CreateWebhook(ctx, h))
		hooks[mask] = h
	}
	bind := f.request(t, "/api/v1/bindings", `{"principal": "user:alice", "role": "admin", "repoMask": "*/*"}`)
	tok := f.request(t, "/api/v1/deploy-tokens", `{"repoMask": "raw/*", "name": "fleet", "scopes": ["write"], "expiresIn": "30d"}`)
	wantStatus(t, f.doAs(http.MethodPost, fmt.Sprintf("/api/v1/approvals/%d/approve", bind.ID), "", root, pass), http.StatusOK)
	wantStatus(t, f.doAs(http.MethodPost, fmt.Sprintf("/api/v1/approvals/%d/reject", tok.ID), "", admin, pass), http.StatusOK)
	for mask, want := range map[string][]string{
		"*/*":      {webhook.TypeApprovalRejected, webhook.TypeApprovalApproved, webhook.TypeApprovalRequested, webhook.TypeApprovalRequested},
		"raw/*":    {webhook.TypeApprovalRejected, webhook.TypeApprovalRequested},
		"raw/open": nil,
	} {
		ds, err := f.st.ListWebhookDeliveries(ctx, hooks[mask].ID, 0, 10)
		must(t, err)
		var got []string
		for _, d := range ds {
			got = append(got, d.EventType)
			for _, never := range []string{tok.Secret, auth.HashSecret(tok.Secret), `"hash"`, `"credential"`, `"signIn"`} {
				if strings.Contains(string(d.Payload), never) {
					t.Errorf("an event to %s holds %q: %s", mask, never, d.Payload)
				}
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("the events to the webhook of %s: %v, want %v", mask, got, want)
		}
	}
}
