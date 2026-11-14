package storetest

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// newWebhook creates an active webhook of mask, created at at(0).
func newWebhook(t *testing.T, s index.Writer, mask string) index.Webhook {
	t.Helper()
	h := index.Webhook{Mask: mask, URL: "https://hooks.example.com/" + mask, SecretEnc: []byte("sealed " + mask), Active: true,
		CreatedBy: "user:alice", CreatedAt: at(0)}
	must(t, s.CreateWebhook(t.Context(), &h))
	return h
}

// testWebhooks: a webhook is created with its uid, read back as written, listed by id, changed
// in its URL, events, secret, active and description but never its mask, uid or creator, and
// deleted with its deliveries.
func testWebhooks(t *testing.T, s index.Store) {
	ctx := t.Context()
	h := index.Webhook{Mask: "apt/ubuntu-internal", URL: "https://ci.example.com/hook", Events: []string{"version.published"},
		SecretEnc: []byte{1, 2, 3}, Active: true, Description: "CI", CreatedBy: "user:alice", CreatedAt: at(0)}
	must(t, s.CreateWebhook(ctx, &h))
	if h.ID == 0 || !index.IsUID(h.UID) || !h.UpdatedAt.Equal(at(0)) {
		t.Fatalf("CreateWebhook filled %+v, want an ID, a uid and UpdatedAt = CreatedAt", h)
	}
	got, err := s.GetWebhook(ctx, h.ID)
	must(t, err)
	same(t, "GetWebhook", h, *got)
	every := newWebhook(t, s, "*/*")
	if every.Events != nil {
		t.Errorf("a webhook of every type: Events = %v, want nil", every.Events)
	}
	list, err := s.ListWebhooks(ctx)
	must(t, err)
	same(t, "ListWebhooks", []int64{h.ID, every.ID}, ids(list, func(w index.Webhook) int64 { return w.ID }))
	same(t, "ListWebhooks of every type", []string(nil), list[1].Events)

	changed := index.Webhook{ID: h.ID, Mask: "*/*", UID: "x", CreatedBy: "user:mallory", URL: "https://ci.example.com/other",
		SecretEnc: []byte{4, 5}, Description: "off"}
	must(t, s.UpdateWebhook(ctx, &changed))
	got, err = s.GetWebhook(ctx, h.ID)
	must(t, err)
	if got.Mask != h.Mask || got.UID != h.UID || got.CreatedBy != h.CreatedBy || !got.CreatedAt.Equal(h.CreatedAt) {
		t.Errorf("UpdateWebhook changed what never changes: %+v", got)
	}
	if got.URL != changed.URL || got.Events != nil || string(got.SecretEnc) != "\x04\x05" || got.Active || got.Description != "off" {
		t.Errorf("UpdateWebhook stored %+v, want the URL, events, secret, active and description of %+v", got, changed)
	}
	same(t, "UpdateWebhook fills the row", *got, changed)
	wantErr(t, "UpdateWebhook of no row", s.UpdateWebhook(ctx, &index.Webhook{ID: 1 << 40}), index.ErrNotFound)

	ds := []index.WebhookDelivery{{WebhookID: h.ID, EventID: "e1", EventType: "version.published", Payload: []byte("{}")}}
	must(t, s.CreateWebhookDeliveries(ctx, ds))
	must(t, s.DeleteWebhook(ctx, h.ID))
	_, err = s.GetWebhook(ctx, h.ID)
	wantErr(t, "GetWebhook after the delete", err, index.ErrNotFound)
	left, err := s.ListWebhookDeliveries(ctx, h.ID, 0, 10)
	must(t, err)
	same(t, "the deliveries of a deleted webhook", 0, len(left))
	wantErr(t, "DeleteWebhook of no row", s.DeleteWebhook(ctx, h.ID), index.ErrNotFound)

	must(t, s.ResetDerived(ctx, 0)) // control plane: left alone
	if _, err := s.GetWebhook(ctx, every.ID); err != nil {
		t.Errorf("ResetDerived took a webhook: %v", err)
	}
}

// testWebhookDeliveries: deliveries are written with the defaults of the outbox, and never by
// a transaction that rolls back; a claim takes the due rows of active webhooks that nobody
// holds, or that a stale claim holds; an outcome is stored only under the claim that took the
// row; the delivery log pages newest first; old rows are deleted whatever their state.
func testWebhookDeliveries(t *testing.T, s index.Store) {
	ctx := t.Context()
	h := newWebhook(t, s, "raw/scratch")
	off := newWebhook(t, s, "raw/*")
	off.Active = false
	must(t, s.UpdateWebhook(ctx, &off))

	// The outbox row of an event is written with its audit row or not at all.
	boom := errors.New("boom")
	err := s.Tx(ctx, func(w index.Writer) error {
		must(t, w.AppendAudit(ctx, &index.AuditEvent{Principal: "user:alice", Action: "push", Repo: "raw/scratch"}))
		must(t, w.CreateWebhookDeliveries(ctx, []index.WebhookDelivery{{WebhookID: h.ID, EventID: "rolled", EventType: "version.published",
			Payload: []byte("{}")}}))
		return boom
	})
	wantErr(t, "a transaction that rolls back", err, boom)
	none, err := s.ListWebhookDeliveries(ctx, h.ID, 0, 10)
	must(t, err)
	same(t, "the deliveries of a rolled-back transaction", 0, len(none))

	ds := []index.WebhookDelivery{
		{WebhookID: h.ID, EventID: "e0", EventType: "version.published", Payload: []byte(`{"n":0}`), CreatedAt: at(0)},
		{WebhookID: h.ID, EventID: "e1", EventType: "version.deleted", Payload: []byte(`{"n":1}`), CreatedAt: at(1)},
		{WebhookID: h.ID, EventID: "e2", EventType: "repo.created", Payload: []byte(`{"n":2}`), CreatedAt: at(2), NextAt: at(100)},
		{WebhookID: off.ID, EventID: "e3", EventType: "repo.created", Payload: []byte(`{"n":3}`), CreatedAt: at(3)},
	}
	must(t, s.Tx(ctx, func(w index.Writer) error { return w.CreateWebhookDeliveries(ctx, ds) }))
	for _, d := range ds {
		if d.ID == 0 || d.State != index.DeliveryPending || d.NextAt.IsZero() || d.Attempts != 0 {
			t.Fatalf("CreateWebhookDeliveries filled %+v, want an ID, pending and NextAt", d)
		}
	}
	same(t, "NextAt of a delivery without one", at(0), ds[0].NextAt)

	log, err := s.ListWebhookDeliveries(ctx, h.ID, 0, 2)
	must(t, err)
	same(t, "the first page of the log", []int64{ds[2].ID, ds[1].ID}, ids(log, func(d index.WebhookDelivery) int64 { return d.ID }))
	same(t, "a delivery as written", ds[2], log[0])
	log, err = s.ListWebhookDeliveries(ctx, h.ID, ds[1].ID, 2)
	must(t, err)
	same(t, "the page after it", []int64{ds[0].ID}, ids(log, func(d index.WebhookDelivery) int64 { return d.ID }))

	// Due at at(10): e0 and e1 of the active webhook; e2 is due later, e3's webhook is inactive.
	claims, err := s.ClaimWebhookDeliveries(ctx, "r1", at(10), at(-110), 10)
	must(t, err)
	same(t, "the rows claimed", []int64{ds[0].ID, ds[1].ID}, claimIDs(claims))
	if c := claims[0]; c.Delivery.ClaimedBy != "r1" || c.Delivery.ClaimedAt == nil || !c.Delivery.ClaimedAt.Equal(at(10)) ||
		c.Webhook.ID != h.ID || string(c.Webhook.SecretEnc) != string(h.SecretEnc) || string(c.Delivery.Payload) != `{"n":0}` {
		t.Errorf("a claim: %+v", c)
	}
	again, err := s.ClaimWebhookDeliveries(ctx, "r2", at(20), at(-100), 10)
	must(t, err)
	same(t, "a claim of rows that a fresh claim holds", 0, len(again))

	// An outcome under the claim: delivered.
	done := claims[0].Delivery
	done.State, done.Attempts, done.LastAt, done.LastStatus, done.DeliveredAt = index.DeliveryDelivered, 1, ptr(at(11)), 204, ptr(at(11))
	must(t, s.FinishWebhookDelivery(ctx, &done))
	if done.ClaimedBy != "" || done.ClaimedAt != nil {
		t.Errorf("FinishWebhookDelivery left the claim on %+v", done)
	}
	wantErr(t, "FinishWebhookDelivery without a claim", s.FinishWebhookDelivery(ctx, &done), index.ErrNotFound)

	// The claim of e1 goes stale and is taken over: the first holder's outcome is dropped.
	taken, err := s.ClaimWebhookDeliveries(ctx, "r2", at(200), at(80), 10)
	must(t, err)
	same(t, "a stale claim taken over, and e2 now due", []int64{ds[1].ID, ds[2].ID}, claimIDs(taken))
	lost := claims[1].Delivery
	lost.State, lost.Attempts, lost.LastStatus, lost.LastError = index.DeliveryFailed, 1, 500, "answered 500"
	wantErr(t, "FinishWebhookDelivery of a claim taken over", s.FinishWebhookDelivery(ctx, &lost), index.ErrNotFound)
	retry := taken[0].Delivery
	retry.Attempts, retry.NextAt, retry.LastAt, retry.LastStatus, retry.LastError = 1, at(230), ptr(at(201)), 503, "answered 503"
	must(t, s.FinishWebhookDelivery(ctx, &retry))
	log, err = s.ListWebhookDeliveries(ctx, h.ID, 0, 10)
	must(t, err)
	got := log[1] // e1
	retry.ClaimedBy, retry.ClaimedAt = "", nil
	same(t, "a delivery due again", retry, got)
	same(t, "a delivered row", done, log[2])
	soon, err := s.ClaimWebhookDeliveries(ctx, "r1", at(229), at(100), 10)
	must(t, err)
	same(t, "a claim before the retry is due", 0, len(soon))

	n, err := s.DeleteWebhookDeliveries(ctx, at(2))
	must(t, err)
	same(t, "DeleteWebhookDeliveries of the rows created before at(2)", 2, n)
	log, err = s.ListWebhookDeliveries(ctx, h.ID, 0, 10)
	must(t, err)
	same(t, "the log after the delete", []int64{ds[2].ID}, ids(log, func(d index.WebhookDelivery) int64 { return d.ID }))
	log, err = s.ListWebhookDeliveries(ctx, h.ID, 0, 0)
	must(t, err)
	same(t, "a page of no row", 0, len(log))
}

// claimIDs returns the delivery ids of claims.
func claimIDs(claims []index.WebhookClaim) []int64 {
	return ids(claims, func(c index.WebhookClaim) int64 { return c.Delivery.ID })
}

// testWebhookClaimRace: replicas that claim at once never claim one row twice, and between
// them claim every due row.
func testWebhookClaimRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	h := newWebhook(t, s, "*/*")
	const rows, replicas = 40, 6
	ds := make([]index.WebhookDelivery, rows)
	for i := range ds {
		ds[i] = index.WebhookDelivery{WebhookID: h.ID, EventID: "e" + strconv.Itoa(i), EventType: "repo.created", Payload: []byte("{}"),
			CreatedAt: at(0)}
	}
	must(t, s.CreateWebhookDeliveries(ctx, ds))
	var (
		mu      sync.Mutex
		claimed = map[int64]string{}
		wg      sync.WaitGroup
	)
	for r := range replicas {
		wg.Go(func() {
			replica := "r" + strconv.Itoa(r)
			for {
				cs, err := s.ClaimWebhookDeliveries(ctx, replica, at(10), at(-110), 3)
				if err != nil {
					if errors.Is(err, index.ErrUnavailable) { // SQLite busy: try again
						time.Sleep(time.Millisecond)
						continue
					}
					t.Error(err)
					return
				}
				if len(cs) == 0 {
					return
				}
				mu.Lock()
				for _, c := range cs {
					if prev, ok := claimed[c.Delivery.ID]; ok {
						t.Errorf("delivery %d claimed by %s and %s", c.Delivery.ID, prev, replica)
					}
					claimed[c.Delivery.ID] = replica
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	same(t, "the rows claimed", rows, len(claimed))
}
