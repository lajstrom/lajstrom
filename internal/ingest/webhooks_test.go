package ingest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// webhookEnv is newEnv with a pipeline whose outbox writes the events of every webhook, and an
// active webhook of raw/* that gets them.
func webhookEnv(t *testing.T) (*env, index.Webhook) {
	t.Helper()
	e := newEnv(t)
	opts := e.options(e.prov)
	opts.Webhooks = &webhook.Outbox{Source: "https://art.example.com"}
	e.pipe = ingest.New(opts)
	h := index.Webhook{Mask: "raw/*", URL: "https://hooks.example.com/", SecretEnc: []byte("sealed"), Active: true,
		CreatedBy: "user:admin"}
	must(t, e.st.CreateWebhook(t.Context(), &h))
	return e, h
}

// eventsOf returns the deliveries of h, oldest first, and their data.
func eventsOf(t *testing.T, st index.Store, h index.Webhook) ([]index.WebhookDelivery, []map[string]any) {
	t.Helper()
	ds, err := st.ListWebhookDeliveries(t.Context(), h.ID, 0, 100)
	must(t, err)
	var data []map[string]any
	for i := len(ds) - 1; i >= 0; i-- {
		var doc struct {
			Data map[string]any `json:"data"`
		}
		must(t, json.Unmarshal(ds[i].Payload, &doc))
		data = append(data, doc.Data)
	}
	for i, j := 0, len(ds)-1; i < j; i, j = i+1, j-1 {
		ds[i], ds[j] = ds[j], ds[i]
	}
	return ds, data
}

// TestPublishWebhook: a publish writes its version.published in the transaction of its audit
// row, with the row's id, the version's keys, digests and files; the same bytes published again
// write no second event.
func TestPublishWebhook(t *testing.T) {
	e, h := webhookEnv(t)
	built, raw, pkgPath := e.prepare(t, upload("notes", "1.0", file{"notes", "hello"}))
	res, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	ds, data := eventsOf(t, e.st, h)
	if len(ds) != 1 || ds[0].EventType != webhook.TypeVersionPublished {
		t.Fatalf("deliveries %+v, want one version.published", ds)
	}
	audit := e.rows(t).Audit[0]
	d := data[0]
	files, _ := d["files"].([]any)
	repo, _ := d["repo"].(map[string]any)
	switch {
	case d["action"] != "push" || d["audit"] != float64(audit.ID) || d["package"] != "notes" || d["version"] != "1.0":
		t.Errorf("data %v, want the push of notes 1.0 with audit row %d", d, audit.ID)
	case d["digest"] != res.Digest.String() || d["tag"] != built.Tag || d["ociRepository"] != "raw/scratch/"+pkgPath:
		t.Errorf("data %v, want digest %s, tag %s", d, res.Digest, built.Tag)
	case repo["format"] != "raw" || repo["name"] != "scratch" || repo["kind"] != "hosted" || len(files) != 1:
		t.Errorf("data %v", d)
	}

	_, err = e.ingest(t, built, raw, pkgPath)
	must(t, err)
	if ds, _ := eventsOf(t, e.st, h); len(ds) != 1 {
		t.Errorf("%d deliveries after the same publish again, want 1", len(ds))
	}
}

// failRelease is a store whose transactions fail at the release of a tag claim, the last write
// of a publish's transaction, after its audit row and its event.
type failRelease struct{ index.Store }

func (s failRelease) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.Store.Tx(ctx, func(w index.Writer) error { return fn(releaseFails{w}) })
}

type releaseFails struct{ index.Writer }

func (releaseFails) ReleaseTagClaim(context.Context, int64) error { return errors.New("boom") }

// TestPublishWebhookRollsBack: a publish whose index transaction rolls back after its event was
// written leaves no delivery, and its audit row failed.
func TestPublishWebhookRollsBack(t *testing.T) {
	e, h := webhookEnv(t)
	opts := e.options(e.prov)
	opts.Index = failRelease{e.st}
	opts.Webhooks = &webhook.Outbox{}
	e.pipe = ingest.New(opts)
	built, raw, pkgPath := e.prepare(t, upload("notes", "1.0", file{"notes", "hello"}))
	if _, err := e.ingest(t, built, raw, pkgPath); err == nil {
		t.Fatal("the publish succeeded")
	}
	if ds, _ := eventsOf(t, e.st, h); len(ds) != 0 {
		t.Errorf("%d deliveries of a publish whose transaction rolled back", len(ds))
	}
	if ev := e.rows(t).Audit[0]; ev.State != index.AuditFailed {
		t.Errorf("the audit row %+v, want failed", ev)
	}
}

// TestDeleteWebhook: a version deleted writes its version.deleted in the transaction of its
// audit row.
func TestDeleteWebhook(t *testing.T) {
	e, h := webhookEnv(t)
	built, raw, pkgPath := e.prepare(t, upload("notes", "1.0", file{"notes", "hello"}))
	res, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	_, err = e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: "1.0"}, alice)
	must(t, err)
	ds, data := eventsOf(t, e.st, h)
	if len(ds) != 2 || ds[1].EventType != webhook.TypeVersionDeleted {
		t.Fatalf("deliveries %+v, want the publish and the delete", ds)
	}
	audit := e.rows(t).Audit[0]
	if d := data[1]; d["action"] != "delete" || d["audit"] != float64(audit.ID) || d["digest"] != res.Digest.String() ||
		d["package"] != "notes" || d["version"] != "1.0" || d["files"] != nil {
		t.Errorf("data %v, want the delete of notes 1.0, audit row %d", d, audit.ID)
	}
}

// failDeliveries is a store whose transactions cannot write deliveries.
type failDeliveries struct{ index.Store }

func (s failDeliveries) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.Store.Tx(ctx, func(w index.Writer) error { return fn(deliveriesFail{w}) })
}

type deliveriesFail struct{ index.Writer }

func (deliveriesFail) CreateWebhookDeliveries(context.Context, []index.WebhookDelivery) error {
	return errors.New("boom")
}

// TestDeleteWebhookFails: a delete whose event cannot be written does not commit its rows
// either: the audit row is failed, and the version stays in the index for reindex to remove.
func TestDeleteWebhookFails(t *testing.T) {
	e, h := webhookEnv(t)
	built, raw, pkgPath := e.prepare(t, upload("notes", "1.0", file{"notes", "hello"}))
	_, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	opts := e.options(e.prov)
	opts.Index = failDeliveries{e.st}
	opts.Webhooks = &webhook.Outbox{}
	if _, err := ingest.New(opts).Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: "1.0"}, alice); err == nil {
		t.Fatal("the delete succeeded")
	}
	rows := e.rows(t)
	if ev := rows.Audit[0]; ev.Action != "delete" || ev.State != index.AuditFailed {
		t.Errorf("the audit row %+v, want the delete, failed", ev)
	}
	if len(rows.Versions) != 1 {
		t.Errorf("versions %+v, want the version kept with the transaction that failed", rows.Versions)
	}
	if ds, _ := eventsOf(t, e.st, h); len(ds) != 1 {
		t.Errorf("%d deliveries, want the publish's only", len(ds))
	}
}
