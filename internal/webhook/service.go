package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// ErrInvalid marks a request whose field breaks its rule; the message names the field.
var ErrInvalid = errors.New("webhook: invalid")

// The audit actions of the changes of a webhook (docs/spec/rbac.md §5).
const (
	ActionCreate = "webhook.create"
	ActionUpdate = "webhook.update"
	ActionDelete = "webhook.delete"
	ActionSecret = "webhook.secret"
	actionTest   = "webhook.test"
)

// maxDescription bounds a webhook's description.
const maxDescription = 256

// Service manages the webhooks of the control plane (docs/spec/webhooks.md §5): the handlers of
// /api/v1/…/webhooks and lajstrom admin webhook go through it, so that the checks and the audit
// rows exist once. Every change is one audit row by auth.PrincipalFrom(ctx), in its transaction.
// Who may is the caller's to decide. Create it as a struct literal; its methods may be called
// concurrently.
type Service struct {
	// Index is the index database; required.
	Index index.Store
	// Deliverer checks the URLs and sends the tests; required.
	Deliverer *Deliverer
	// Master seals and opens the secrets; nil: Create, Rotate and Test are ErrNoMasterKey.
	Master *keys.Master
	// Formats reports whether a format plugin of that name is compiled in, for a mask's format
	// segment ("*" and oci are always taken); nil takes any name.
	Formats func(name string) bool
	// Replica is the id of the replica whose claims a test's row carries.
	Replica string
	// Source is server.externalURL, the base of a ping's source.
	Source string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
}

// Origin is where a change came from, for its audit row: as auth.Origin.
type Origin = auth.Origin

// Request is what Create takes, and the fields Update changes when they are set.
type Request struct {
	Mask        string
	URL         *string
	Events      *[]string
	Active      *bool
	Description *string
}

// Create checks req and stores the webhook with a new secret, sealed with the master key, and
// its webhook.create row; it returns the webhook and the secret's text, the one time it is
// shown. A field that breaks its rule wraps ErrInvalid and is named.
func (s *Service) Create(ctx context.Context, req Request, o Origin) (*index.Webhook, string, error) {
	if s.Master == nil {
		return nil, "", ErrNoMasterKey
	}
	if err := s.checkMask(req.Mask); err != nil {
		return nil, "", err
	}
	if req.URL == nil {
		return nil, "", invalidf("url: required")
	}
	now := s.now()
	h := index.Webhook{UID: index.NewUID(now), Mask: req.Mask, Active: true, CreatedBy: auth.PrincipalFrom(ctx).String(),
		CreatedAt: now}
	if err := s.apply(&h, req); err != nil {
		return nil, "", err
	}
	text, key := NewSecret()
	sealed, err := Seal(s.Master, h.UID, key)
	if err != nil {
		return nil, "", err
	}
	h.SecretEnc = sealed
	err = s.Index.Tx(ctx, func(w index.Writer) error {
		if err := w.CreateWebhook(ctx, &h); err != nil {
			return err
		}
		return s.record(ctx, w, ActionCreate, &h, o)
	})
	if err != nil {
		return nil, "", fmt.Errorf("webhook: create of %s: %w", req.Mask, err)
	}
	return &h, text, nil
}

// Get returns the webhook id, or an error wrapping index.ErrNotFound.
func (s *Service) Get(ctx context.Context, id int64) (*index.Webhook, error) {
	return s.Index.GetWebhook(ctx, id)
}

// List returns the webhooks whose mask is mask, or every one when mask is "", by id.
func (s *Service) List(ctx context.Context, mask string) ([]index.Webhook, error) {
	all, err := s.Index.ListWebhooks(ctx)
	if err != nil || mask == "" {
		return all, err
	}
	return slices.DeleteFunc(all, func(h index.Webhook) bool { return h.Mask != mask }), nil
}

// Update changes the fields req sets of the webhook id, with its webhook.update row; req.Mask
// set is ErrInvalid: a mask never changes.
func (s *Service) Update(ctx context.Context, id int64, req Request, o Origin) (*index.Webhook, error) {
	if req.Mask != "" {
		return nil, invalidf("mask: cannot change; delete the webhook and create one of the other mask")
	}
	var h *index.Webhook
	err := s.Index.Tx(ctx, func(w index.Writer) (err error) {
		if h, err = w.GetWebhook(ctx, id); err != nil {
			return err
		}
		if err := s.apply(h, req); err != nil {
			return err
		}
		if err := w.UpdateWebhook(ctx, h); err != nil {
			return err
		}
		return s.record(ctx, w, ActionUpdate, h, o)
	})
	if err != nil {
		return nil, s.wrap("update", id, err)
	}
	return h, nil
}

// Delete deletes the webhook id and its delivery log, with its webhook.delete row.
func (s *Service) Delete(ctx context.Context, id int64, o Origin) error {
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		h, err := w.GetWebhook(ctx, id)
		if err != nil {
			return err
		}
		if err := w.DeleteWebhook(ctx, id); err != nil {
			return err
		}
		return s.record(ctx, w, ActionDelete, h, o)
	})
	return s.wrap("delete", id, err)
}

// Rotate gives the webhook id a new secret, with its webhook.secret row, and returns its text,
// the one time it is shown; the old secret signs nothing from the commit on.
func (s *Service) Rotate(ctx context.Context, id int64, o Origin) (string, error) {
	if s.Master == nil {
		return "", ErrNoMasterKey
	}
	text, key := NewSecret()
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		h, err := w.GetWebhook(ctx, id)
		if err != nil {
			return err
		}
		if h.SecretEnc, err = Seal(s.Master, h.UID, key); err != nil {
			return err
		}
		if err := w.UpdateWebhook(ctx, h); err != nil {
			return err
		}
		return s.record(ctx, w, ActionSecret, h, o)
	})
	if err != nil {
		return "", s.wrap("rotate the secret of", id, err)
	}
	return text, nil
}

// Test sends a webhook.ping to the webhook id now, active or not, once, and returns its row of
// the delivery log: delivered or failed (webhooks.md §5).
func (s *Service) Test(ctx context.Context, id int64, requestID string) (*index.WebhookDelivery, error) {
	if s.Master == nil {
		return nil, ErrNoMasterKey
	}
	h, err := s.Index.GetWebhook(ctx, id)
	if err != nil {
		return nil, s.wrap("test", id, err)
	}
	key, err := Open(s.Master, *h)
	if err != nil {
		return nil, err
	}
	p := auth.PrincipalFrom(ctx)
	data := PingData{Common: Common{Schema: Schema, Action: actionTest, Actor: Actor{Principal: p.String()}, RequestID: requestID}}
	data.Webhook.ID, data.Webhook.Mask = h.ID, h.Mask
	if p.Kind == auth.KindUser {
		if u, err := s.Index.GetUserByUID(ctx, p.Name); err == nil {
			data.Actor.Name = u.Name
		}
	}
	at := s.now()
	eventID, body, err := (&Outbox{}).document(Event{Type: TypePing, Data: data}, at,
		s.Source+"/api/v1/webhooks/"+strconv.FormatInt(h.ID, 10))
	if err != nil {
		return nil, err
	}
	ds := []index.WebhookDelivery{{WebhookID: h.ID, EventID: eventID, EventType: TypePing, Payload: body, CreatedAt: at,
		ClaimedBy: s.replica(), ClaimedAt: &at}}
	if err := s.Index.CreateWebhookDeliveries(ctx, ds); err != nil {
		return nil, s.wrap("test", id, err)
	}
	d := ds[0]
	res, err := s.Deliverer.Attempt(ctx, h.URL, key, eventID, body)
	if err != nil {
		return nil, err
	}
	Settle(&d, res, s.now(), 1)
	if err := s.Index.FinishWebhookDelivery(context.WithoutCancel(ctx), &d); err != nil {
		return nil, s.wrap("record the test of", id, err)
	}
	return &d, nil
}

// Deliveries returns a page of the delivery log of the webhook id: the rows below before (0:
// from the newest), newest first, at most limit.
func (s *Service) Deliveries(ctx context.Context, id, before int64, limit int) ([]index.WebhookDelivery, error) {
	if _, err := s.Index.GetWebhook(ctx, id); err != nil {
		return nil, s.wrap("list the deliveries of", id, err)
	}
	return s.Index.ListWebhookDeliveries(ctx, id, before, limit)
}

// apply checks and writes on h the fields req sets.
func (s *Service) apply(h *index.Webhook, req Request) error {
	if req.URL != nil {
		if err := s.Deliverer.CheckURL(*req.URL); err != nil {
			return invalidf("url: %s", err)
		}
		h.URL = *req.URL
	}
	if req.Events != nil {
		events := slices.Clone(*req.Events)
		slices.Sort(events)
		events = slices.Compact(events)
		for _, e := range events {
			if !IsType(e) {
				return invalidf("events: %q is not one of %v", e, Types)
			}
		}
		h.Events = events
		if len(events) == 0 {
			h.Events = nil
		}
	}
	if req.Active != nil {
		h.Active = *req.Active
	}
	if req.Description != nil {
		if len(*req.Description) > maxDescription {
			return invalidf("description: longer than %d bytes", maxDescription)
		}
		h.Description = *req.Description
	}
	return nil
}

// checkMask checks a mask of docs/spec/rbac.md §3: two segments, each "*" or a name, the
// format one of this build.
func (s *Service) checkMask(mask string) error {
	f, _, err := auth.ParseMask(mask)
	if err != nil {
		return invalidf("mask: %s", err)
	}
	if f != "*" && f != "oci" && s.Formats != nil && !s.Formats(f) {
		return invalidf("mask: %q is not a format of this build", f)
	}
	return nil
}

// record appends the audit row of a change of h on w: repo the mask, subject webhook:<id>,
// the details of rbac.md §5 (the host of the URL, never the URL) and the origin.
func (s *Service) record(ctx context.Context, w index.Writer, action string, h *index.Webhook, o Origin) error {
	details := map[string]any{"mask": h.Mask, "urlHost": hostOf(h.URL), "events": h.Events, "active": h.Active}
	for k, v := range map[string]string{"source": o.Source, "requestId": o.RequestID, "host": o.Host} {
		if v != "" {
			details[k] = v
		}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	ev := index.AuditEvent{At: s.now(), Principal: auth.PrincipalFrom(ctx).String(), Action: action, Repo: h.Mask,
		Subject: "webhook:" + strconv.FormatInt(h.ID, 10), IP: o.IP, Details: raw}
	return w.AppendAudit(ctx, &ev)
}

// wrap turns an error about the webhook id into one for the callers: ErrInvalid as it is,
// index.ErrNotFound with a message for the person, anything else wrapped with op.
func (s *Service) wrap(op string, id int64, err error) error {
	switch {
	case err == nil, errors.Is(err, ErrInvalid), errors.Is(err, ErrNoMasterKey):
		return err
	case errors.Is(err, index.ErrNotFound):
		return fmt.Errorf("webhook %d does not exist: %w", id, err)
	}
	return fmt.Errorf("webhook: %s webhook %d: %w", op, id, err)
}

// invalidError is a field that breaks its rule; its message, for the client, names the field.
type invalidError struct{ msg string }

func (e *invalidError) Error() string { return e.msg }

// Is reports whether target is ErrInvalid.
func (e *invalidError) Is(target error) bool { return target == ErrInvalid }

func invalidf(msg string, args ...any) error { return &invalidError{msg: fmt.Sprintf(msg, args...)} }

// replica is the claim of a test's row: the replica, or "test" outside serve.
func (s *Service) replica() string {
	if s.Replica != "" {
		return s.Replica
	}
	return "test"
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
