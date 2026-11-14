package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Webhooks (docs/spec/webhooks.md, docs/spec/index-schema.md, Webhooks): the webhooks and their
// deliveries, both control plane. A delivery row is the outbox of one event for one webhook and
// its line of the delivery log.

const webhookCols = "id, uid, mask, url, events, secret_enc, active, description, created_by, created_at, updated_at"

func webhookDests(h *index.Webhook) []any {
	return []any{&h.ID, &h.UID, &h.Mask, &h.URL, jsonDest[[]string]{&h.Events}, blobDest{&h.SecretEnc}, &h.Active,
		&h.Description, &h.CreatedBy, timeDest{&h.CreatedAt}, timeDest{&h.UpdatedAt}}
}

// GetWebhook returns the webhook with id.
func (c *conn) GetWebhook(ctx context.Context, id int64) (*index.Webhook, error) {
	return getOne(ctx, c, fmt.Sprintf("webhooks id %d", id), webhookDests,
		"SELECT "+webhookCols+" FROM webhooks WHERE id = ?", id)
}

// ListWebhooks returns every webhook ordered by id.
func (c *conn) ListWebhooks(ctx context.Context) ([]index.Webhook, error) {
	return getAll(ctx, c, "webhooks", webhookDests, "SELECT "+webhookCols+" FROM webhooks ORDER BY id")
}

// CreateWebhook inserts h; UID is made when empty, CreatedAt is now when zero and UpdatedAt is
// CreatedAt.
func (c *conn) CreateWebhook(ctx context.Context, h *index.Webhook) error {
	created := stamp(&h.CreatedAt, now())
	h.UpdatedAt = created
	if h.UID == "" {
		h.UID = index.NewUID(created)
	}
	return c.scanRow(ctx, "webhooks of "+h.Mask,
		"INSERT INTO webhooks (uid, mask, url, events, secret_enc, active, description, created_by, created_at, updated_at)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		[]any{h.UID, h.Mask, h.URL, eventsArg(h.Events), blobArg(h.SecretEnc), h.Active, h.Description, h.CreatedBy,
			c.d.EncodeTime(h.CreatedAt), c.d.EncodeTime(h.UpdatedAt)},
		&h.ID)
}

// UpdateWebhook stores URL, Events, SecretEnc, Active and Description of h by h.ID, sets
// UpdatedAt, and fills the columns that never change from the row.
func (c *conn) UpdateWebhook(ctx context.Context, h *index.Webhook) error {
	what := fmt.Sprintf("webhooks id %d", h.ID)
	h.UpdatedAt = now()
	return c.withTx(ctx, func(c *conn) error {
		if err := c.execOne(ctx, what,
			"UPDATE webhooks SET url = ?, events = ?, secret_enc = ?, active = ?, description = ?, updated_at = ? WHERE id = ?",
			h.URL, eventsArg(h.Events), blobArg(h.SecretEnc), h.Active, h.Description, c.d.EncodeTime(h.UpdatedAt),
			h.ID); err != nil {
			return err
		}
		row, err := c.GetWebhook(ctx, h.ID)
		if err != nil {
			return err
		}
		h.UID, h.Mask, h.CreatedBy, h.CreatedAt = row.UID, row.Mask, row.CreatedBy, row.CreatedAt
		return nil
	})
}

// DeleteWebhook removes the webhook; its deliveries go with it (ON DELETE CASCADE).
func (c *conn) DeleteWebhook(ctx context.Context, id int64) error {
	return c.execOne(ctx, fmt.Sprintf("webhooks id %d", id), "DELETE FROM webhooks WHERE id = ?", id)
}

// eventsArg marshals the types of a webhook for its nullable json column: empty is NULL, every
// type.
func eventsArg(events []string) any {
	if len(events) == 0 {
		return nil
	}
	return scopesArg(events)
}

const deliveryCols = "id, webhook_id, event_id, event_type, payload, state, attempts, next_at, claimed_by, claimed_at," +
	" last_at, last_status, last_error, created_at, delivered_at"

func deliveryDests(d *index.WebhookDelivery) []any {
	return []any{&d.ID, &d.WebhookID, &d.EventID, &d.EventType, blobDest{&d.Payload}, &d.State, &d.Attempts,
		timeDest{&d.NextAt}, textDest{&d.ClaimedBy}, nullTimeDest{&d.ClaimedAt}, nullTimeDest{&d.LastAt},
		nullIntDest{&d.LastStatus}, textDest{&d.LastError}, timeDest{&d.CreatedAt}, nullTimeDest{&d.DeliveredAt}}
}

// ListWebhookDeliveries returns the deliveries of a webhook with an id below before (0: every
// one), newest first, at most limit.
func (c *conn) ListWebhookDeliveries(ctx context.Context, webhookID, before int64, limit int) ([]index.WebhookDelivery, error) {
	if limit < 1 {
		return nil, nil
	}
	w := where{}
	w.add("webhook_id = ?", webhookID)
	if before > 0 {
		w.add("id < ?", before)
	}
	return getAll(ctx, c, fmt.Sprintf("webhook_deliveries of %d", webhookID), deliveryDests,
		"SELECT "+deliveryCols+" FROM webhook_deliveries"+w.clause()+" ORDER BY id DESC LIMIT ?", append(w.args, limit)...)
}

// CreateWebhookDeliveries inserts ds in one transaction (the caller's, when it runs in one):
// State pending when "", CreatedAt now when zero, NextAt CreatedAt when zero.
func (c *conn) CreateWebhookDeliveries(ctx context.Context, ds []index.WebhookDelivery) error {
	if len(ds) == 0 {
		return nil
	}
	return c.withTx(ctx, func(c *conn) error {
		for i := range ds {
			d := &ds[i]
			created := stamp(&d.CreatedAt, now())
			stamp(&d.NextAt, created)
			if d.State == "" {
				d.State = index.DeliveryPending
			}
			d.ClaimedAt, d.LastAt, d.DeliveredAt = stampNull(d.ClaimedAt), stampNull(d.LastAt), stampNull(d.DeliveredAt)
			d.LastError = auditError(d.LastError)
			if err := c.scanRow(ctx, fmt.Sprintf("webhook_deliveries of %d", d.WebhookID),
				"INSERT INTO webhook_deliveries (webhook_id, event_id, event_type, payload, state, attempts, next_at,"+
					" claimed_by, claimed_at, last_at, last_status, last_error, created_at, delivered_at)"+
					" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
				[]any{d.WebhookID, d.EventID, d.EventType, blobArg(d.Payload), d.State, d.Attempts, c.d.EncodeTime(d.NextAt),
					textArg(d.ClaimedBy), c.d.encodeNullTime(d.ClaimedAt), c.d.encodeNullTime(d.LastAt), statusArg(d.LastStatus),
					textArg(d.LastError), c.d.EncodeTime(d.CreatedAt), c.d.encodeNullTime(d.DeliveredAt)},
				&d.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// ClaimWebhookDeliveries claims due deliveries for replica in one transaction: it selects them
// with their webhooks, on Postgres locking them and skipping the rows another transaction locks,
// then marks them claimed.
func (c *conn) ClaimWebhookDeliveries(ctx context.Context, replica string, at, staleBefore time.Time, limit int) ([]index.WebhookClaim, error) {
	if limit < 1 {
		return nil, nil
	}
	at, staleBefore = Normalize(at), Normalize(staleBefore)
	query := "SELECT " + qualify("d", deliveryCols) + ", " + qualify("h", webhookCols) +
		" FROM webhook_deliveries d JOIN webhooks h ON h.id = d.webhook_id" +
		" WHERE d.state = ? AND d.next_at <= ? AND h.active = ? AND (d.claimed_at IS NULL OR d.claimed_at < ?)" +
		" ORDER BY d.next_at, d.id LIMIT ?"
	if c.d.Name == index.DialectPostgres {
		query += " FOR UPDATE OF d SKIP LOCKED"
	}
	var claims []index.WebhookClaim
	err := c.withTx(ctx, func(c *conn) error {
		var err error
		claims, err = getAll(ctx, c, "webhook_deliveries due", func(cl *index.WebhookClaim) []any {
			return append(deliveryDests(&cl.Delivery), webhookDests(&cl.Webhook)...)
		}, query, index.DeliveryPending, c.d.EncodeTime(at), true, c.d.EncodeTime(staleBefore), limit)
		if err != nil || len(claims) == 0 {
			return err
		}
		ids := make([]string, len(claims))
		args := []any{replica, c.d.EncodeTime(at)}
		for i := range claims {
			ids[i] = "?"
			args = append(args, claims[i].Delivery.ID)
			claimedAt := at
			claims[i].Delivery.ClaimedBy, claims[i].Delivery.ClaimedAt = replica, &claimedAt
		}
		_, err = c.exec(ctx, "webhook_deliveries due",
			"UPDATE webhook_deliveries SET claimed_by = ?, claimed_at = ? WHERE id IN ("+strings.Join(ids, ", ")+")", args...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// FinishWebhookDelivery stores the outcome of an attempt and clears the claim, in one UPDATE
// that holds only while the row's claim is d's.
func (c *conn) FinishWebhookDelivery(ctx context.Context, d *index.WebhookDelivery) error {
	what := fmt.Sprintf("webhook_deliveries id %d", d.ID)
	if d.ClaimedAt == nil || d.ClaimedBy == "" {
		return fmt.Errorf("%w: %s is not claimed", index.ErrNotFound, what)
	}
	d.NextAt, d.LastAt, d.DeliveredAt = Normalize(d.NextAt), stampNull(d.LastAt), stampNull(d.DeliveredAt)
	d.LastError = auditError(d.LastError)
	claimedAt := Normalize(*d.ClaimedAt)
	n, err := c.exec(ctx, what,
		"UPDATE webhook_deliveries SET state = ?, attempts = ?, next_at = ?, last_at = ?, last_status = ?, last_error = ?,"+
			" delivered_at = ?, claimed_by = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ? AND claimed_at = ?",
		d.State, d.Attempts, c.d.EncodeTime(d.NextAt), c.d.encodeNullTime(d.LastAt), statusArg(d.LastStatus),
		textArg(d.LastError), c.d.encodeNullTime(d.DeliveredAt), d.ID, d.ClaimedBy, c.d.EncodeTime(claimedAt))
	switch {
	case err != nil:
		return err
	case n == 0:
		return fmt.Errorf("%w: %s: the claim of %s is not the row's any more", index.ErrNotFound, what, d.ClaimedBy)
	}
	d.ClaimedBy, d.ClaimedAt = "", nil
	return nil
}

// DeleteWebhookDeliveries removes the deliveries created before before, whatever their state.
func (c *conn) DeleteWebhookDeliveries(ctx context.Context, before time.Time) (int, error) {
	return c.exec(ctx, "webhook_deliveries", "DELETE FROM webhook_deliveries WHERE created_at < ?",
		c.d.EncodeTime(Normalize(before)))
}

// statusArg is the value of last_status: 0, no answer, is NULL.
func statusArg(status int) any {
	if status == 0 {
		return nil
	}
	return status
}

// nullIntDest scans a nullable integer column: NULL is 0.
type nullIntDest struct{ n *int }

// Scan implements sql.Scanner.
func (d nullIntDest) Scan(src any) error {
	var n sql.NullInt64
	if err := n.Scan(src); err != nil {
		return err
	}
	*d.n = int(n.Int64)
	return nil
}
