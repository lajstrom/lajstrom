package webhook_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

var memoryDBs atomic.Int64

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(t.Context(), fmt.Sprintf("file:webhook-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(t.Context()))
	return st
}

func newMaster(t *testing.T) *keys.Master {
	t.Helper()
	m, err := keys.ParseMaster(strings.Repeat("ab", 32))
	must(t, err)
	return m
}

// hook creates a webhook of mask to target with a new secret sealed by m, and returns it with
// its key.
func hook(t *testing.T, st index.Store, m *keys.Master, mask, target string, events ...string) (index.Webhook, []byte) {
	t.Helper()
	_, key := webhook.NewSecret()
	h := index.Webhook{UID: index.NewUID(t0), Mask: mask, URL: target, Events: events, Active: true, CreatedBy: "user:alice"}
	sealed, err := webhook.Seal(m, h.UID, key)
	must(t, err)
	h.SecretEnc = sealed
	must(t, st.CreateWebhook(t.Context(), &h))
	return h, key
}

var scratch = index.Repo{Format: "raw", Name: "scratch", Kind: index.KindHosted}

// published is the event of a publish of notes 1.0 to raw/scratch, by the audit row ev.
func published(ev index.AuditEvent) webhook.Event {
	return webhook.Event{Type: webhook.TypeVersionPublished, Repo: scratch, Subject: "notes 1.0", Data: webhook.VersionData{
		Common: webhook.CommonOf(ev), Repo: webhook.RefOf(scratch), Package: "notes", Version: "1.0", Tag: "1.0",
		Digest: "sha256:aa", ArtifactType: "application/vnd.lajstrom.raw.v1", OCIRepository: "raw/scratch/notes",
		Files: []webhook.File{{Name: "notes", MediaType: "application/vnd.lajstrom.raw.file.v1", Digest: "sha256:bb", Size: 5}}}}
}

// hooks are the webhooks of the outbox tests: two that get a publish of raw/scratch, three that
// do not (another mask, other events, inactive).
type hooks struct{ exact, wild, other, deletes, off index.Webhook }

func newHooks(t *testing.T, st index.Store, m *keys.Master) hooks {
	t.Helper()
	var h hooks
	h.exact, _ = hook(t, st, m, "raw/scratch", "https://a.example.com/")
	h.wild, _ = hook(t, st, m, "*/*", "https://b.example.com/", webhook.TypeVersionPublished)
	h.other, _ = hook(t, st, m, "raw/other", "https://c.example.com/")
	h.deletes, _ = hook(t, st, m, "raw/*", "https://d.example.com/", webhook.TypeVersionDeleted)
	h.off, _ = hook(t, st, m, "raw/scratch", "https://e.example.com/")
	h.off.Active = false
	must(t, st.UpdateWebhook(t.Context(), &h.off))
	return h
}

var outbox = &webhook.Outbox{Source: "https://art.example.com", Now: func() time.Time { return t0 }}

// publish appends the audit row of a publish and enqueues its event, in one transaction that
// returns fail; it returns the audit row.
func publish(t *testing.T, st index.Store, fail error) (index.AuditEvent, error) {
	t.Helper()
	var ev index.AuditEvent
	err := st.Tx(t.Context(), func(w index.Writer) error {
		ev = index.AuditEvent{Principal: "user:alice", Action: "push", Repo: "raw/scratch", Subject: "notes 1.0",
			Details: json.RawMessage(`{"requestId":"req-1"}`)}
		if err := w.AppendAudit(t.Context(), &ev); err != nil {
			return err
		}
		if err := outbox.Enqueue(t.Context(), w, published(ev)); err != nil {
			return err
		}
		return fail
	})
	return ev, err
}

// deliveries returns the delivery log of h.
func deliveries(t *testing.T, st index.Store, h index.Webhook) []index.WebhookDelivery {
	t.Helper()
	ds, err := st.ListWebhookDeliveries(t.Context(), h.ID, 0, 100)
	must(t, err)
	return ds
}

// TestEnqueueRollsBack: the outbox rows of an event go with its audit row's transaction: none
// when it rolls back.
func TestEnqueueRollsBack(t *testing.T) {
	st := newStore(t)
	h := newHooks(t, st, newMaster(t))
	boom := errors.New("boom")
	if _, err := publish(t, st, boom); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	for _, w := range []index.Webhook{h.exact, h.wild} {
		if ds := deliveries(t, st, w); len(ds) != 0 {
			t.Errorf("webhook %s: %d deliveries of a rolled-back transaction", w.Mask, len(ds))
		}
	}
}

// TestEnqueueMatches: one delivery per webhook that matches the event, with one event id and
// body; none for a webhook of another mask, of other events, or inactive, and none at all when
// nothing matches.
func TestEnqueueMatches(t *testing.T) {
	ctx := t.Context()
	st := newStore(t)
	h := newHooks(t, st, newMaster(t))
	_, err := publish(t, st, nil)
	must(t, err)
	a, b := deliveries(t, st, h.exact), deliveries(t, st, h.wild)
	if len(a) != 1 || len(b) != 1 || a[0].State != index.DeliveryPending || a[0].EventType != webhook.TypeVersionPublished {
		t.Fatalf("deliveries %+v and %+v, want one pending version.published each", a, b)
	}
	if a[0].EventID != b[0].EventID || string(a[0].Payload) != string(b[0].Payload) {
		t.Errorf("one event, two ids or bodies: %s, %s", a[0].EventID, b[0].EventID)
	}
	for _, w := range []index.Webhook{h.other, h.deletes, h.off} {
		if ds := deliveries(t, st, w); len(ds) != 0 {
			t.Errorf("webhook %s (events %v, active %v) got the event", w.Mask, w.Events, w.Active)
		}
	}
	none := webhook.Event{Type: webhook.TypeRepoCreated, Repo: index.Repo{Format: "npm", Name: "x"}, Data: webhook.RepoData{}}
	must(t, st.Tx(ctx, func(w index.Writer) error { return outbox.Enqueue(ctx, w, none) }))
	var nilOutbox *webhook.Outbox
	must(t, nilOutbox.Enqueue(ctx, st, none))
}

// TestEventDocument: the body is the CloudEvents document of webhooks.md §2 and §3, and holds
// nothing of what it must never hold.
func TestEventDocument(t *testing.T) {
	st := newStore(t)
	h := newHooks(t, st, newMaster(t))
	audit, err := publish(t, st, nil)
	must(t, err)
	d := deliveries(t, st, h.exact)[0]
	var doc map[string]any
	must(t, json.Unmarshal(d.Payload, &doc))
	data, _ := doc["data"].(map[string]any)
	actor, _ := data["actor"].(map[string]any)
	for _, f := range []struct {
		name      string
		got, want any
	}{
		{"specversion", doc["specversion"], "1.0"},
		{"id", doc["id"], d.EventID},
		{"source", doc["source"], "https://art.example.com/raw/scratch"},
		{"type", doc["type"], "dev.lajstrom.version.published"},
		{"subject", doc["subject"], "notes 1.0"},
		{"time", doc["time"], "2026-11-14T10:00:00Z"},
		{"datacontenttype", doc["datacontenttype"], "application/json"},
		{"data.schema", data["schema"], 1.0},
		{"data.action", data["action"], "push"},
		{"data.audit", data["audit"], float64(audit.ID)},
		{"data.requestId", data["requestId"], "req-1"},
		{"data.actor.principal", actor["principal"], "user:alice"},
		{"data.digest", data["digest"], "sha256:aa"},
		{"data.ociRepository", data["ociRepository"], "raw/scratch/notes"},
	} {
		if f.got != f.want {
			t.Errorf("%s = %v, want %v", f.name, f.got, f.want)
		}
	}
	if !index.IsUID(d.EventID) {
		t.Errorf("event id %q is not a UUID", d.EventID)
	}
	for _, never := range []string{"ip", "secret", "url", "headers", "metadata", "details"} {
		if _, ok := data[never]; ok {
			t.Errorf("data has %q", never)
		}
	}
}

// counter is a receiver that counts the deliveries of each event and answers with answer.
type counter struct {
	mu     sync.Mutex
	got    map[string]int
	answer func(n int) int // the status of the n-th request, from 1
	n      int
}

func (c *counter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.n++
	code := c.answer(c.n)
	if code/100 == 2 {
		c.got[r.Header.Get("webhook-id")]++
	}
	c.mu.Unlock()
	w.WriteHeader(code)
}

// enqueue writes n events of raw/scratch.
func enqueue(t *testing.T, st index.Store, n int) {
	t.Helper()
	outbox := &webhook.Outbox{Now: func() time.Time { return t0 }}
	for i := range n {
		must(t, st.Tx(t.Context(), func(w index.Writer) error {
			ev := index.AuditEvent{Principal: "user:alice", Action: "push", Repo: "raw/scratch", Subject: fmt.Sprint("notes ", i)}
			if err := w.AppendAudit(t.Context(), &ev); err != nil {
				return err
			}
			return outbox.Enqueue(t.Context(), w, published(ev))
		}))
	}
}

// TestWorkersShareTheOutbox: the workers of two replicas on one database deliver every event,
// each once while no claim goes stale, and record every row delivered.
func TestWorkersShareTheOutbox(t *testing.T) {
	ctx := t.Context()
	st, m := newStore(t), newMaster(t)
	c := &counter{got: map[string]int{}, answer: func(int) int { return http.StatusNoContent }}
	rec := newReceiver(t, c.ServeHTTP)
	h, _ := hook(t, st, m, "raw/*", rec.URL)
	const events = 30
	enqueue(t, st, events)
	clock := func() time.Time { return t0.Add(time.Second) }
	workers := []*webhook.Worker{
		{Index: st, Deliverer: allowed(t, time.Second), Master: m, Replica: "replica-a", Workers: 3, Now: clock},
		{Index: st, Deliverer: allowed(t, time.Second), Master: m, Replica: "replica-b", Workers: 3, Now: clock},
	}
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Go(func() {
			for {
				n, err := w.RunOnce(ctx)
				if err != nil && !errors.Is(err, index.ErrUnavailable) {
					t.Error(err)
					return
				}
				if n == 0 && err == nil {
					return
				}
			}
		})
	}
	wg.Wait()
	if len(c.got) != events {
		t.Errorf("%d events delivered, want %d", len(c.got), events)
	}
	for id, n := range c.got {
		if n != 1 {
			t.Errorf("event %s delivered %d times", id, n)
		}
	}
	ds, err := st.ListWebhookDeliveries(ctx, h.ID, 0, 100)
	must(t, err)
	for _, d := range ds {
		if d.State != index.DeliveryDelivered || d.Attempts != 1 || d.ClaimedBy != "" || d.LastStatus != http.StatusNoContent {
			t.Errorf("delivery %d: %+v", d.ID, d)
		}
	}
}

// TestWorkerTakesOverAStaleClaim: a replica that claimed a delivery and stopped leaves it to
// another, once its claim is 2 minutes old, and the first one's outcome, when it comes back, is
// dropped.
func TestWorkerTakesOverAStaleClaim(t *testing.T) {
	ctx := t.Context()
	st, m := newStore(t), newMaster(t)
	c := &counter{got: map[string]int{}, answer: func(int) int { return http.StatusOK }}
	rec := newReceiver(t, c.ServeHTTP)
	hook(t, st, m, "raw/scratch", rec.URL)
	enqueue(t, st, 1)
	dead, err := st.ClaimWebhookDeliveries(ctx, "replica-dead", t0, t0.Add(-webhook.ClaimStale), 10)
	must(t, err)
	if len(dead) != 1 {
		t.Fatalf("claimed %d, want 1", len(dead))
	}
	w := &webhook.Worker{Index: st, Deliverer: allowed(t, time.Second), Master: m, Replica: "replica-live"}
	w.Now = func() time.Time { return t0.Add(time.Minute) }
	if n, err := w.RunOnce(ctx); err != nil || n != 0 {
		t.Errorf("RunOnce under a fresh claim = %d, %v; want nothing claimed", n, err)
	}
	w.Now = func() time.Time { return t0.Add(webhook.ClaimStale + time.Second) }
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce after the claim went stale = %d, %v; want 1", n, err)
	}
	late := dead[0].Delivery
	webhook.Settle(&late, webhook.Result{Status: 500, Error: "answered 500"}, t0.Add(3*time.Minute), 8)
	if err := st.FinishWebhookDelivery(ctx, &late); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the dead replica's outcome: %v, want ErrNotFound", err)
	}
	if len(c.got) != 1 {
		t.Errorf("deliveries %v", c.got)
	}
}

// TestWorkerRetries: a receiver that fails is sent the event again once the backoff has passed,
// with the same id and body and a fresh signature, until it answers 2xx.
func TestWorkerRetries(t *testing.T) {
	ctx := t.Context()
	st, m := newStore(t), newMaster(t)
	c := &counter{got: map[string]int{}, answer: func(n int) int {
		if n < 3 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}}
	rec := newReceiver(t, c.ServeHTTP)
	h, key := hook(t, st, m, "raw/scratch", rec.URL)
	enqueue(t, st, 1)
	now := t0
	w := &webhook.Worker{Index: st, Deliverer: allowed(t, time.Second), Master: m, Replica: "r", MaxAttempts: 5,
		Now: func() time.Time { return now }}
	for attempt := 1; attempt <= 3; attempt++ {
		if n, err := w.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("attempt %d: claimed %d, %v", attempt, n, err)
		}
		verify(t, rec, key)
		d := deliveries(t, st, h)[0]
		switch {
		case d.Attempts != attempt:
			t.Fatalf("attempts %d, want %d", d.Attempts, attempt)
		case attempt == 3 && (d.State != index.DeliveryDelivered || d.LastStatus != 200):
			t.Errorf("after attempt 3: %+v, want delivered", d)
		case attempt < 3 && (d.State != index.DeliveryPending || d.LastStatus != 503 || !d.NextAt.After(now)):
			t.Fatalf("after attempt %d: %+v, want pending, due later", attempt, d)
		case attempt < 3:
			if n, _ := w.RunOnce(ctx); n != 0 {
				t.Fatal("a delivery claimed before its backoff passed")
			}
			now = d.NextAt
		}
	}
}

// verify fails the test when the next request rec got does not carry the signature of key.
func verify(t *testing.T, rec *receiver, key []byte) {
	t.Helper()
	req, body := <-rec.reqs, <-rec.bodies
	if want := webhook.Sign(key, req.Header.Get("webhook-id"), t0.Unix(), body); req.Header.Get("webhook-signature") != want {
		t.Error("the signature does not verify")
	}
}

// TestWorkerGivesUp: a receiver that never answers 2xx has its delivery failed after
// maxAttempts.
func TestWorkerGivesUp(t *testing.T) {
	ctx := t.Context()
	st, m := newStore(t), newMaster(t)
	never := &counter{got: map[string]int{}, answer: func(int) int { return http.StatusInternalServerError }}
	rec := newReceiver(t, never.ServeHTTP)
	h, _ := hook(t, st, m, "npm/*", rec.URL)
	must(t, st.Tx(ctx, func(wr index.Writer) error {
		return (&webhook.Outbox{}).Enqueue(ctx, wr, webhook.Event{Type: webhook.TypeRepoCreated,
			Repo: index.Repo{Format: "npm", Name: "x", Kind: index.KindHosted}, Data: webhook.RepoData{}})
	}))
	now := t0
	w := &webhook.Worker{Index: st, Deliverer: allowed(t, time.Second), Master: m, Replica: "r", MaxAttempts: 2,
		Now: func() time.Time { return now }}
	for range 3 {
		now = now.Add(2 * time.Hour)
		_, err := w.RunOnce(ctx)
		must(t, err)
	}
	if d := deliveries(t, st, h)[0]; d.State != index.DeliveryFailed || d.Attempts != 2 || d.LastStatus != 500 || never.n != 2 {
		t.Errorf("%+v after %d requests, want failed after 2", d, never.n)
	}
}

// TestWorkerWithoutTheMasterKey: a secret that does not open with the replica's master key is
// a failed attempt, tried again; the request is never sent unsigned.
func TestWorkerWithoutTheMasterKey(t *testing.T) {
	ctx := t.Context()
	st := newStore(t)
	c := &counter{got: map[string]int{}, answer: func(int) int { return http.StatusOK }}
	rec := newReceiver(t, c.ServeHTTP)
	h, _ := hook(t, st, newMaster(t), "raw/scratch", rec.URL)
	enqueue(t, st, 1)
	other, err := keys.ParseMaster(strings.Repeat("cd", 32))
	must(t, err)
	w := &webhook.Worker{Index: st, Deliverer: allowed(t, time.Second), Master: other, Replica: "r", Now: func() time.Time { return t0 }}
	_, err = w.RunOnce(ctx)
	must(t, err)
	ds, err := st.ListWebhookDeliveries(ctx, h.ID, 0, 1)
	must(t, err)
	if d := ds[0]; d.State != index.DeliveryPending || d.Attempts != 1 || !strings.Contains(d.LastError, "master key") || c.n != 0 {
		t.Errorf("%+v, %d requests; want a failed attempt and no request", d, c.n)
	}
}

// TestLogsNoSecret: at every level, the logs of a delivery that fails, of one refused and of a
// failure for good name the webhook and its host, never its secret, its URL's path and query,
// or the address its host resolved to (docs/spec/guarantees.md, INV-21).
func TestLogsNoSecret(t *testing.T) {
	ctx := t.Context()
	st, m := newStore(t), newMaster(t)
	var logs strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: &logs, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := &counter{got: map[string]int{}, answer: func(int) int { return http.StatusBadGateway }}
	rec := newReceiver(t, c.ServeHTTP)
	failing, key := hook(t, st, m, "raw/scratch", rec.URL+"/hooks/tok-s3cr3t?sig=q-s3cr3t")
	refused, key2 := hook(t, st, m, "raw/*", "https://localhost:1/hooks/tok-s3cr3t")
	enqueue(t, st, 1)
	d := webhook.NewDeliverer(webhook.Options{AllowedHosts: []string{"127.0.0.1"}, Timeout: time.Second, Log: log,
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }})
	w := &webhook.Worker{Index: st, Deliverer: d, Master: m, Replica: "r", MaxAttempts: 1, Log: log,
		Now: func() time.Time { return t0 }}
	_, err := w.RunOnce(ctx)
	must(t, err)
	if got := deliveries(t, st, failing)[0]; got.State != index.DeliveryFailed {
		t.Fatalf("the failing delivery: %+v", got)
	}
	if got := deliveries(t, st, refused)[0]; got.State != index.DeliveryFailed || strings.Contains(got.LastError, "127.0.0.1") {
		t.Fatalf("the refused delivery: %+v", got)
	}
	mu.Lock()
	out := logs.String()
	mu.Unlock()
	if !strings.Contains(out, "failed for good") || !strings.Contains(out, "host=127.0.0.1") {
		t.Errorf("the log does not say which webhook failed:\n%s", out)
	}
	for _, secret := range []string{"tok-s3cr3t", "q-s3cr3t", base64.StdEncoding.EncodeToString(key),
		base64.StdEncoding.EncodeToString(key2)} {
		if strings.Contains(out, secret) {
			t.Errorf("the log holds %q:\n%s", secret, out)
		}
	}
}

// lockedWriter serialises the writes of the workers' goroutines to one builder.
type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestSettle: an attempt's outcome on its delivery: delivered on a 2xx, failed when refused or
// on the last attempt, else due again after the backoff.
func TestSettle(t *testing.T) {
	for _, tt := range []struct {
		name     string
		before   int // attempts already made
		res      webhook.Result
		state    string
		attempts int
	}{
		{"a 503", 0, webhook.Result{Status: 503, Error: "answered 503"}, index.DeliveryPending, 1},
		{"the last attempt", 2, webhook.Result{Error: "timeout"}, index.DeliveryFailed, 3},
		{"a refused target", 0, webhook.Result{Error: "refused: the host has no public address", Refused: true}, index.DeliveryFailed, 1},
		{"a delivery", 1, webhook.Result{Delivered: true, Status: 200}, index.DeliveryDelivered, 2},
	} {
		d := index.WebhookDelivery{State: index.DeliveryPending, Attempts: tt.before, NextAt: t0}
		webhook.Settle(&d, tt.res, t0, 3)
		if d.State != tt.state || d.Attempts != tt.attempts || d.LastStatus != tt.res.Status || d.LastError != tt.res.Error ||
			d.LastAt == nil || !d.LastAt.Equal(t0) {
			t.Errorf("%s: %+v, want %s after %d attempts", tt.name, d, tt.state, tt.attempts)
		}
		if delivered := d.DeliveredAt != nil; delivered != (tt.state == index.DeliveryDelivered) {
			t.Errorf("%s: DeliveredAt %v", tt.name, d.DeliveredAt)
		}
		if tt.state == index.DeliveryPending && (d.NextAt.Before(t0.Add(30*time.Second)) || d.NextAt.After(t0.Add(33*time.Second))) {
			t.Errorf("%s: due at %s, want 30 to 33 s later", tt.name, d.NextAt)
		}
	}
}
