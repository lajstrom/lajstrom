package api

import (
	"context"
	"encoding/json"
	"testing"

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
