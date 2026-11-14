package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// Webhook is the resource of a webhook (docs/api.md, Webhooks): never its secret.
type Webhook struct {
	ID          int64     `json:"id"`
	Mask        string    `json:"mask"`
	URL         string    `json:"url"`
	Events      []string  `json:"events"`
	Active      bool      `json:"active"`
	Description string    `json:"description"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// CreatedWebhook is the answer of a POST: the webhook and its secret, shown this once.
type CreatedWebhook struct {
	Webhook
	Secret string `json:"secret"`
}

// WebhookList is the answer of GET …/webhooks.
type WebhookList struct {
	Webhooks []Webhook `json:"webhooks"`
}

// Secret is the answer of POST …/webhooks/{id}/secret.
type Secret struct {
	Secret string `json:"secret"`
}

// Delivery is a row of a webhook's delivery log.
type Delivery struct {
	ID          int64      `json:"id"`
	EventID     string     `json:"eventId"`
	EventType   string     `json:"eventType"`
	State       string     `json:"state"`
	Attempts    int        `json:"attempts"`
	NextAt      *time.Time `json:"nextAt,omitempty"`
	LastAt      *time.Time `json:"lastAt,omitempty"`
	LastStatus  int        `json:"lastStatus,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
}

// DeliveryList is a page of the delivery log, newest first, and the cursor of the next page.
type DeliveryList struct {
	Deliveries []Delivery `json:"deliveries"`
	Next       string     `json:"next,omitempty"`
}

// webhookBody is the body of POST and PATCH: mask only in a POST of /api/v1/webhooks.
type webhookBody struct {
	Mask        *string   `json:"mask"`
	URL         *string   `json:"url"`
	Events      *[]string `json:"events"`
	Active      *bool     `json:"active"`
	Description *string   `json:"description"`
}

// WebhookOf is the resource of h, for the API and the CLI.
func WebhookOf(h *index.Webhook) Webhook {
	events := h.Events
	if events == nil {
		events = []string{}
	}
	return Webhook{ID: h.ID, Mask: h.Mask, URL: h.URL, Events: events, Active: h.Active, Description: h.Description,
		CreatedBy: h.CreatedBy, CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt}
}

// DeliveryOf is the resource of a row of a delivery log, for the API and the CLI.
func DeliveryOf(d *index.WebhookDelivery) Delivery {
	r := Delivery{ID: d.ID, EventID: d.EventID, EventType: d.EventType, State: d.State, Attempts: d.Attempts, LastAt: d.LastAt,
		LastStatus: d.LastStatus, LastError: d.LastError, CreatedAt: d.CreatedAt, DeliveredAt: d.DeliveredAt}
	if d.State == index.DeliveryPending {
		next := d.NextAt
		r.NextAt = &next
	}
	return r
}

// webhookScope is where a webhook route was asked: under a repo (its mask) or under
// /api/v1/webhooks (every mask, the admin role).
type webhookScope struct {
	mask string // "" under /api/v1/webhooks
}

// webhookScopeOf decides whether the request may manage the webhooks of its route: manage on
// the repo, before the repo is looked up (docs/spec/rbac.md §3), and then the repo must exist;
// the admin role under /api/v1/webhooks, before the body is read.
func (h *handler) webhookScopeOf(r *http.Request) (webhookScope, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if f == "" {
		return webhookScope{}, h.allow(r, auth.ActionManage, auth.System, auth.System)
	}
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return webhookScope{}, err
	}
	if _, err := h.repos.Get(r.Context(), f, name); err != nil {
		return webhookScope{}, err
	}
	return webhookScope{mask: f + "/" + name}, nil
}

// webhookOf returns the webhook {id} of the route's scope: under a repo, a webhook of another
// mask is 404, as one that does not exist.
func (h *handler) webhookOf(r *http.Request) (webhookScope, *index.Webhook, error) {
	scope, err := h.webhookScopeOf(r)
	if err != nil {
		return scope, nil, err
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return scope, nil, &clientError{msg: fmt.Sprintf("webhook %q does not exist", r.PathValue("id")), err: index.ErrNotFound}
	}
	wh, err := h.webhooks.Get(r.Context(), id)
	if err == nil && scope.mask != "" && wh.Mask != scope.mask {
		err = index.ErrNotFound
	}
	if err != nil {
		return scope, nil, webhookError(id, err)
	}
	return scope, wh, nil
}

// listWebhooks answers the webhooks of the scope, by id.
func (h *handler) listWebhooks(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	scope, err := h.webhookScopeOf(r)
	if err != nil {
		return 0, nil, err
	}
	all, err := h.webhooks.List(r.Context(), scope.mask)
	if err != nil {
		return 0, nil, err
	}
	list := WebhookList{Webhooks: make([]Webhook, len(all))}
	for i := range all {
		list.Webhooks[i] = WebhookOf(&all[i])
	}
	return http.StatusOK, list, nil
}

// createWebhook creates a webhook from the body: 201 with it, its secret and its Location.
func (h *handler) createWebhook(w http.ResponseWriter, r *http.Request) (int, any, error) {
	scope, err := h.webhookScopeOf(r)
	if err != nil {
		return 0, nil, err
	}
	var body webhookBody
	if err := decodeBody(w, r, &body); err != nil {
		return 0, nil, err
	}
	mask := scope.mask
	switch {
	case scope.mask != "" && body.Mask != nil && *body.Mask != scope.mask:
		return 0, nil, invalid("mask: a webhook of this route has the mask %s; leave it out", scope.mask)
	case scope.mask == "" && body.Mask == nil:
		return 0, nil, invalid("mask: required, a repo mask such as apt/* or */*")
	case scope.mask == "":
		mask = *body.Mask
	}
	wh, secret, err := h.webhooks.Create(r.Context(), webhook.Request{Mask: mask, URL: body.URL, Events: body.Events,
		Active: body.Active, Description: body.Description}, auth.Origin(origin(r)))
	if err != nil {
		return 0, nil, webhookError(0, err)
	}
	w.Header().Set("Location", webhookPath(scope, wh.ID))
	return http.StatusCreated, CreatedWebhook{Webhook: WebhookOf(wh), Secret: secret}, nil
}

// getWebhook answers one webhook.
func (h *handler) getWebhook(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	_, wh, err := h.webhookOf(r)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, WebhookOf(wh), nil
}

// updateWebhook changes url, events, active and description; mask is 400.
func (h *handler) updateWebhook(w http.ResponseWriter, r *http.Request) (int, any, error) {
	_, wh, err := h.webhookOf(r)
	if err != nil {
		return 0, nil, err
	}
	var body webhookBody
	if err := decodeBody(w, r, &body); err != nil {
		return 0, nil, err
	}
	req := webhook.Request{URL: body.URL, Events: body.Events, Active: body.Active, Description: body.Description}
	if body.Mask != nil {
		req.Mask = *body.Mask
		if req.Mask == "" {
			req.Mask = " " // set but empty: still a change of the mask
		}
	}
	updated, err := h.webhooks.Update(r.Context(), wh.ID, req, auth.Origin(origin(r)))
	if err != nil {
		return 0, nil, webhookError(wh.ID, err)
	}
	return http.StatusOK, WebhookOf(updated), nil
}

// deleteWebhook deletes a webhook and its delivery log: 204.
func (h *handler) deleteWebhook(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	_, wh, err := h.webhookOf(r)
	if err != nil {
		return 0, nil, err
	}
	if err := h.webhooks.Delete(r.Context(), wh.ID, auth.Origin(origin(r))); err != nil {
		return 0, nil, webhookError(wh.ID, err)
	}
	return http.StatusNoContent, nil, nil
}

// rotateWebhookSecret gives the webhook a new secret and answers it, once.
func (h *handler) rotateWebhookSecret(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	_, wh, err := h.webhookOf(r)
	if err != nil {
		return 0, nil, err
	}
	secret, err := h.webhooks.Rotate(r.Context(), wh.ID, auth.Origin(origin(r)))
	if err != nil {
		return 0, nil, webhookError(wh.ID, err)
	}
	return http.StatusOK, Secret{Secret: secret}, nil
}

// testWebhook sends a ping now and answers its row of the delivery log.
func (h *handler) testWebhook(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	_, wh, err := h.webhookOf(r)
	if err != nil {
		return 0, nil, err
	}
	d, err := h.webhooks.Test(r.Context(), wh.ID, server.RequestID(r.Context()))
	if err != nil {
		return 0, nil, webhookError(wh.ID, err)
	}
	return http.StatusOK, DeliveryOf(d), nil
}

// listDeliveries answers a page of a webhook's delivery log, newest first.
func (h *handler) listDeliveries(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	_, wh, err := h.webhookOf(r)
	if err != nil {
		return 0, nil, err
	}
	limit, after, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	var before int64
	if after != "" {
		if before, err = strconv.ParseInt(after, 10, 64); err != nil || before <= 0 {
			return 0, nil, invalid("cursor: %q is not the next of a page of this list", r.URL.Query().Get("cursor"))
		}
	}
	ds, err := h.webhooks.Deliveries(r.Context(), wh.ID, before, limit+1)
	if err != nil {
		return 0, nil, webhookError(wh.ID, err)
	}
	list := DeliveryList{Deliveries: []Delivery{}}
	for i := range ds {
		if i == limit {
			list.Next = cursor(strconv.FormatInt(ds[i-1].ID, 10))
			break
		}
		list.Deliveries = append(list.Deliveries, DeliveryOf(&ds[i]))
	}
	return http.StatusOK, list, nil
}

// webhookPath is the path of the webhook id in the scope's routes.
func webhookPath(scope webhookScope, id int64) string {
	if scope.mask == "" {
		return fmt.Sprintf("/api/v1/webhooks/%d", id)
	}
	return fmt.Sprintf("/api/v1/repos/%s/webhooks/%d", scope.mask, id)
}

// webhookError turns an error of the webhook service into the one the API answers.
func webhookError(id int64, err error) error {
	switch {
	case errors.Is(err, webhook.ErrInvalid):
		return &clientError{msg: err.Error(), err: format.ErrInvalid}
	case errors.Is(err, webhook.ErrNoMasterKey):
		return &clientError{msg: "webhooks need the master key: set keys.masterKeyEnv or keys.masterKeyFile", err: format.ErrUnsupported}
	case errors.Is(err, index.ErrNotFound):
		return &clientError{msg: fmt.Sprintf("webhook %d does not exist", id), err: index.ErrNotFound}
	}
	return err
}
