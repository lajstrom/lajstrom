package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// The audit actions of a change of a CI trust policy of the API (docs/spec/rbac.md §11).
const (
	ActionPolicyCreate  = "ci.policy.create"
	ActionPolicyUpdate  = "ci.policy.update"
	ActionPolicyRenew   = "ci.policy.renew"
	ActionPolicyApprove = "ci.policy.approve"
	ActionPolicyExpire  = "ci.policy.expire"
	ActionPolicyDelete  = "ci.policy.delete"
)

// The sources of a policy as the API shows it: its owner (ADR 0015).
const (
	SourceConfig = "config"
	SourceAPI    = "api"
)

// WriteLifetime is the longest a policy of the API that grants write lives from its change, and
// its expiry when the change names none (docs/spec/rbac.md §11).
const WriteLifetime = 180 * 24 * time.Hour

// Policy is a CI trust policy as the API shows it and takes it (docs/spec/http-routing.md §4).
type Policy struct {
	Name   string              `json:"name"`
	Issuer string              `json:"issuer"`
	Claims map[string][]string `json:"claims"`
	Grants []index.CIGrant     `json:"grants"`
	// ExpiresAt is when a policy of the API that grants write stops counting. In a body, the
	// expiry asked for one, at most WriteLifetime ahead; none asks WriteLifetime.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Source is SourceConfig or SourceAPI; State is index.CIPolicyActive, CIPolicyPending or
	// CIPolicyExpired, a policy of the configuration always active. The API ignores both, and
	// everything below, in a body.
	Source string `json:"source"`
	State  string `json:"state"`
	// Renewal is the renewal that waits for an admin's approval.
	Renewal *Renewal `json:"renewal,omitempty"`
	// CreatedBy, CreatedAt, UpdatedBy, UpdatedAt, ApprovedBy and ApprovedAt are those of a policy
	// of the API: UpdatedBy made its latest create, change or renewal, ApprovedBy its approval.
	CreatedBy  string     `json:"createdBy,omitempty"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
	UpdatedBy  string     `json:"updatedBy,omitempty"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	ApprovedBy string     `json:"approvedBy,omitempty"`
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
	// ID and Revision are those of the row of a policy of the API, for its ETag; 0 for one of
	// the configuration.
	ID       int64 `json:"-"`
	Revision int64 `json:"-"`
}

// Renewal is a renewal of a policy that waits for an admin's approval: the expiry it asks.
type Renewal struct {
	ExpiresAt time.Time `json:"expiresAt"`
}

// Service makes, changes, renews, approves, expires and deletes the CI trust policies of the
// API, and lists them with those of the configuration (docs/spec/rbac.md §11): the API and
// lajstrom admin ci-policy go through it, so that the rules, the audit rows and the events exist
// once. Who may call it (the admin role) is the caller's to decide; the steps of a policy that
// grants write (a fresh sign-in with a second factor, a second admin, auth.ci.apiWriteRepos, an
// expiry) are its own, read from the principal and the credential of the context. Create it as a
// struct literal; its methods may be called concurrently.
//
// Errors for the caller wrap auth.ErrInvalid (a rule broken, each field named), auth.ErrScope (a
// step of a policy that writes missing: the sign-in, the repos, the approver),
// index.ErrNotFound or index.ErrConflict (a policy of the configuration, a name taken, nothing
// that waits, a change meanwhile), with a message for the person, or the error of the condition
// of the context (index.WithCondition, the If-Match of the API); any other error is a failure of
// the index.
type Service struct {
	// Index holds the policies of the API; required.
	Index index.Store
	// Config is auth.ci: its issuers, its policies, auth.ci.apiWriteRepos and
	// auth.ci.apiWriteApproval.
	Config config.CI
	// Webhooks writes the event of each change in its transaction; nil writes none.
	Webhooks *webhook.Outbox
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// serviceError is an error of the Service for the caller: its message, and the sentinel it wraps.
type serviceError struct {
	msg string
	err error
}

func (e *serviceError) Error() string { return e.msg }
func (e *serviceError) Unwrap() error { return e.err }

// List returns the policies of the configuration, in its order, then those of the API, by id.
func (s *Service) List(ctx context.Context) ([]Policy, error) {
	rows, err := s.Index.ListCIPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("ci: list the trust policies: %w", err)
	}
	return Views(s.Config.Policies, rows, s.now()), nil
}

// Views returns the policies of the configuration, configured, in its order, then those of the
// API, rows, in theirs, as List answers them at now: the access report reads its rows in one
// transaction of its own (docs/spec/rbac.md §14).
func Views(configured []config.CIPolicy, rows []index.CIPolicy, now time.Time) []Policy {
	list := make([]Policy, 0, len(configured)+len(rows))
	for _, p := range configured {
		list = append(list, fromConfig(p))
	}
	for _, r := range rows {
		list = append(list, fromRow(r, now))
	}
	return list
}

// Get returns the policy called name, of the configuration or of the API.
func (s *Service) Get(ctx context.Context, name string) (*Policy, error) {
	if p, ok := s.configPolicy(name); ok {
		pol := fromConfig(p)
		return &pol, nil
	}
	r, err := s.row(ctx, s.Index, name)
	if err != nil {
		return nil, err
	}
	pol := fromRow(*r, s.now())
	return &pol, nil
}

// Create makes the policy p of the API, by the principal of ctx, with its ci.policy.create row and
// event. One that grants write takes the steps of writeSteps, and waits pending for a second
// admin unless auth.ci.apiWriteApproval is none; one that reads only takes no expiry.
func (s *Service) Create(ctx context.Context, p Policy, o auth.Origin) (*Policy, error) {
	if err := s.check(p); err != nil {
		return nil, err
	}
	if _, ok := s.configPolicy(p.Name); ok {
		return nil, &serviceError{msg: fmt.Sprintf("policy %s is a policy of the configuration (auth.ci.policies)", p.Name), err: index.ErrConflict}
	}
	by, now := auth.PrincipalFrom(ctx).String(), s.now()
	r := index.CIPolicy{Name: p.Name, Issuer: p.Issuer, Claims: p.Claims, Grants: p.Grants, State: index.CIPolicyActive,
		CreatedBy: by, UpdatedBy: by, CreatedAt: now}
	if err := s.steps(ctx, &r, p.ExpiresAt, now); err != nil {
		return nil, err
	}
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		if err := w.CreateCIPolicy(ctx, &r); err != nil {
			return err
		}
		return s.changed(ctx, w, ActionPolicyCreate, webhook.TypeCIPolicyCreated, r, nil, o)
	})
	switch {
	case errors.Is(err, index.ErrConflict):
		return nil, &serviceError{msg: fmt.Sprintf("policy %s exists already", p.Name), err: err}
	case err != nil:
		return nil, fmt.Errorf("ci: create the trust policy %s: %w", p.Name, err)
	}
	pol := fromRow(r, now)
	return &pol, nil
}

// Update replaces the issuer, the claims and the grants of the policy of the API called name by
// those of p, by the principal of ctx, with its ci.policy.update row and event. A change that
// leaves it granting write takes the steps of Create, a renewal that waited included; one that
// leaves it reading only is active at once, without an expiry. A policy of the configuration is
// index.ErrConflict. The condition of ctx (index.WithCondition) is checked on the row the
// transaction read.
func (s *Service) Update(ctx context.Context, name string, p Policy, o auth.Origin) (*Policy, error) {
	if p.Name != "" && p.Name != name {
		return nil, &serviceError{msg: fmt.Sprintf("name: %q is not the policy's, %s: a policy is not renamed", p.Name, name), err: auth.ErrInvalid}
	}
	p.Name = name
	if err := s.readOnly(name); err != nil {
		return nil, err
	}
	if err := s.check(p); err != nil {
		return nil, err
	}
	now := s.now()
	next := index.CIPolicy{Issuer: p.Issuer, Claims: p.Claims, Grants: p.Grants, State: index.CIPolicyActive,
		UpdatedBy: auth.PrincipalFrom(ctx).String()}
	if err := s.steps(ctx, &next, p.ExpiresAt, now); err != nil {
		return nil, err
	}
	r, err := s.change(ctx, name, ActionPolicyUpdate, webhook.TypeCIPolicyUpdated, o, func(old *index.CIPolicy) (index.CIPolicy, error) {
		r := next
		r.ID, r.Revision = old.ID, old.Revision
		return r, nil
	})
	if err != nil {
		return nil, err
	}
	pol := fromRow(r, now)
	return &pol, nil
}

// Delete deletes the policy of the API called name, with its ci.policy.delete row and event; it
// takes no step, a policy that writes included. A policy of the configuration is
// index.ErrConflict. The condition of ctx (index.WithCondition) is checked on the row the
// transaction read.
func (s *Service) Delete(ctx context.Context, name string, o auth.Origin) error {
	if err := s.readOnly(name); err != nil {
		return err
	}
	err := s.Index.Tx(index.ConditionContext(ctx), func(w index.Writer) error {
		old, err := s.row(ctx, w, name)
		if err != nil {
			return err
		}
		if err := index.CheckCondition(ctx, old.ID, old.Revision); err != nil {
			return err
		}
		if err := w.DeleteCIPolicy(ctx, old.ID); err != nil {
			return err
		}
		return s.changed(ctx, w, ActionPolicyDelete, webhook.TypeCIPolicyDeleted, *old, nil, o)
	})
	return s.txError("delete", name, err)
}

// check applies the rules of a policy of the configuration to p (config.CheckCIPolicy).
func (s *Service) check(p Policy) error {
	if errs := config.CheckCIPolicy(s.Config.Issuers, toConfig(p)); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return &serviceError{msg: strings.Join(msgs, "; "), err: auth.ErrInvalid}
	}
	return nil
}

// readOnly returns index.ErrConflict when name is a policy of the configuration.
func (s *Service) readOnly(name string) error {
	if _, ok := s.configPolicy(name); ok {
		return &serviceError{msg: fmt.Sprintf("policy %s is declared in the configuration (auth.ci.policies): it changes there", name),
			err: index.ErrConflict}
	}
	return nil
}

// configPolicy returns the policy of the configuration called name.
func (s *Service) configPolicy(name string) (config.CIPolicy, bool) {
	i := slices.IndexFunc(s.Config.Policies, func(p config.CIPolicy) bool { return p.Name == name })
	if i < 0 {
		return config.CIPolicy{}, false
	}
	return s.Config.Policies[i], true
}

// row returns the policy of the API called name, read on r.
func (s *Service) row(ctx context.Context, r index.Reader, name string) (*index.CIPolicy, error) {
	p, err := r.GetCIPolicy(ctx, name)
	if errors.Is(err, index.ErrNotFound) {
		return nil, &serviceError{msg: fmt.Sprintf("policy %s does not exist", name), err: err}
	}
	return p, err
}

// txError is err of the change op of the policy name, for the caller.
func (s *Service) txError(op, name string, err error) error {
	var se *serviceError
	if err == nil || errors.As(err, &se) {
		return err
	}
	return fmt.Errorf("ci: %s the trust policy %s: %w", op, name, err)
}

// changed appends on w, in the transaction of the change, the audit row of the change action of
// the policy r and its event of typ (docs/spec/webhooks.md §1): by the principal of ctx, subject
// the name, details the policy as the change left it, the previous one of a change, the sign-in
// of the request's credential when it has one, and the origin; the event to the webhooks of the
// admin that cover a mask of its grants, before or after.
func (s *Service) changed(ctx context.Context, w index.Writer, action, typ string, r index.CIPolicy, previous *index.CIPolicy,
	o auth.Origin) error {
	details := map[string]any{"issuer": r.Issuer, "claims": r.Claims, "grants": r.Grants, "state": r.State}
	if r.ExpiresAt != nil {
		details["expiresAt"] = r.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if r.RenewalExpiresAt != nil {
		details["renewalExpiresAt"] = r.RenewalExpiresAt.UTC().Format(time.RFC3339)
	}
	if previous != nil {
		details["previous"] = map[string]any{"issuer": previous.Issuer, "claims": previous.Claims, "grants": previous.Grants,
			"state": previous.State}
	}
	raw, err := json.Marshal(o.AddTo(auth.AddSignIn(ctx, details)))
	if err != nil {
		return fmt.Errorf("ci: audit details of %s: %w", action, err)
	}
	ev := index.AuditEvent{At: s.now(), Principal: auth.PrincipalFrom(ctx).String(), Action: action, Subject: r.Name, IP: o.IP,
		Details: raw}
	if err := w.AppendAudit(ctx, &ev); err != nil {
		return err
	}
	masks := []string{}
	for _, g := range r.Grants {
		masks = append(masks, g.Mask)
	}
	if previous != nil {
		for _, g := range previous.Grants {
			masks = append(masks, g.Mask)
		}
	}
	return s.Webhooks.Enqueue(ctx, w, webhook.Event{Type: typ, Subject: r.Name, Masks: masks,
		Data: webhook.CIPolicyData{Common: webhook.CommonOf(ev), Policy: webhook.CIPolicyOf(r)}})
}

// now is the clock of the Service.
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// fromConfig is the policy p of the configuration as the API shows it: always active.
func fromConfig(p config.CIPolicy) Policy {
	pol := Policy{Name: p.Name, Issuer: p.Issuer, Claims: map[string][]string{}, Grants: []index.CIGrant{}, Source: SourceConfig,
		State: index.CIPolicyActive}
	for _, c := range slices.Sorted(maps.Keys(p.Claims)) {
		pol.Claims[c] = slices.Clone(p.Claims[c])
	}
	for _, g := range p.Grants {
		pol.Grants = append(pol.Grants, index.CIGrant{Role: g.Role, Mask: g.Mask})
	}
	return pol
}

// fromRow is the row r as the API shows it at now: a policy whose expiry has passed is expired,
// whether a replica marked it so yet or not (Expire).
func fromRow(r index.CIPolicy, now time.Time) Policy {
	created, updated := r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	pol := Policy{Name: r.Name, Issuer: r.Issuer, Claims: r.Claims, Grants: r.Grants, ExpiresAt: utc(r.ExpiresAt), Source: SourceAPI,
		State: r.State, CreatedBy: r.CreatedBy, CreatedAt: &created, UpdatedBy: r.UpdatedBy, UpdatedAt: &updated,
		ApprovedBy: r.ApprovedBy, ApprovedAt: utc(r.ApprovedAt), ID: r.ID, Revision: r.Revision}
	if expired(r, now) {
		pol.State = index.CIPolicyExpired
	}
	if r.RenewalExpiresAt != nil {
		pol.Renewal = &Renewal{ExpiresAt: r.RenewalExpiresAt.UTC()}
	}
	return pol
}

// expired reports whether the policy r grants nothing for its expiry at now: marked expired, or
// past an expiry that a replica has not marked yet.
func expired(r index.CIPolicy, now time.Time) bool {
	return r.State == index.CIPolicyExpired || (r.ExpiresAt != nil && !r.ExpiresAt.After(now))
}

// utc is t in UTC, nil for nil.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// toConfig is p as a policy of the configuration, for its rules.
func toConfig(p Policy) config.CIPolicy {
	return configPolicy(index.CIPolicy{Name: p.Name, Issuer: p.Issuer, Claims: p.Claims, Grants: p.Grants})
}
