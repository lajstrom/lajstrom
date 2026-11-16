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
)

// The audit actions of a change of a CI trust policy through the API (docs/spec/rbac.md §11).
const (
	ActionPolicyCreate = "ci.policy.create"
	ActionPolicyUpdate = "ci.policy.update"
	ActionPolicyDelete = "ci.policy.delete"
)

// The sources of a policy as the API shows it: its owner (ADR 0015).
const (
	SourceConfig = "config"
	SourceAPI    = "api"
)

// Policy is a CI trust policy as the API shows it and takes it (docs/spec/http-routing.md §4).
type Policy struct {
	Name   string              `json:"name"`
	Issuer string              `json:"issuer"`
	Claims map[string][]string `json:"claims"`
	Grants []index.CIGrant     `json:"grants"`
	// Source is SourceConfig or SourceAPI; the API ignores it in a body.
	Source string `json:"source"`
	// CreatedBy, CreatedAt and UpdatedAt are those of a policy of the API.
	CreatedBy string     `json:"createdBy,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// Service makes, changes and deletes the CI trust policies of the API, and lists them with those
// of the configuration (docs/spec/rbac.md §11): the API and lajstrom admin ci-policy go through
// it, so that the rules and the audit rows exist once. Who may call it (the admin role) is the
// caller's to decide. Create it as a struct literal; its methods may be called concurrently.
//
// Errors for the caller wrap auth.ErrInvalid (a rule broken, each field named), auth.ErrScope (a
// policy that writes: a policy of the API reads only), index.ErrNotFound or
// index.ErrConflict (a policy of the configuration, or a name taken), with a message for the
// person; any other error is a failure of the index.
type Service struct {
	// Index holds the policies of the API; required.
	Index index.Store
	// Config is auth.ci: its issuers and its policies.
	Config config.CI
	// Now is the clock of the audit rows; nil is time.Now.
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
	list := make([]Policy, 0, len(s.Config.Policies))
	for _, p := range s.Config.Policies {
		list = append(list, fromConfig(p))
	}
	rows, err := s.Index.ListCIPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("ci: list the trust policies: %w", err)
	}
	for _, r := range rows {
		list = append(list, fromRow(r))
	}
	return list, nil
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
	pol := fromRow(*r)
	return &pol, nil
}

// Create makes the policy p of the API, by the principal of ctx, with its ci.policy.create row.
func (s *Service) Create(ctx context.Context, p Policy, o auth.Origin) (*Policy, error) {
	if err := s.check(p); err != nil {
		return nil, err
	}
	if _, ok := s.configPolicy(p.Name); ok {
		return nil, &serviceError{msg: fmt.Sprintf("policy %s is a policy of the configuration (auth.ci.policies)", p.Name), err: index.ErrConflict}
	}
	by := auth.PrincipalFrom(ctx).String()
	r := index.CIPolicy{Name: p.Name, Issuer: p.Issuer, Claims: p.Claims, Grants: p.Grants, State: index.CIPolicyActive, CreatedBy: by,
		UpdatedBy: by}
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		if err := w.CreateCIPolicy(ctx, &r); err != nil {
			return err
		}
		return s.record(ctx, w, ActionPolicyCreate, r, nil, o)
	})
	switch {
	case errors.Is(err, index.ErrConflict):
		return nil, &serviceError{msg: fmt.Sprintf("policy %s exists already", p.Name), err: err}
	case err != nil:
		return nil, fmt.Errorf("ci: create the trust policy %s: %w", p.Name, err)
	}
	pol := fromRow(r)
	return &pol, nil
}

// Update replaces the issuer, the claims and the grants of the policy of the API called name by
// those of p, by the principal of ctx, with its ci.policy.update row, while the row is at the
// revision read (index.ErrConflict otherwise, a change meanwhile). A policy of the configuration
// is index.ErrConflict.
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
	var r index.CIPolicy
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		old, err := s.row(ctx, w, name)
		if err != nil {
			return err
		}
		r = *old
		r.Issuer, r.Claims, r.Grants, r.UpdatedBy = p.Issuer, p.Claims, p.Grants, auth.PrincipalFrom(ctx).String()
		if err := w.UpdateCIPolicy(ctx, &r); err != nil {
			if errors.Is(err, index.ErrConflict) {
				return &serviceError{msg: fmt.Sprintf("policy %s changed meanwhile: read it again", name), err: err}
			}
			return err
		}
		return s.record(ctx, w, ActionPolicyUpdate, r, old, o)
	})
	if err != nil {
		return nil, s.txError("change", name, err)
	}
	pol := fromRow(r)
	return &pol, nil
}

// Delete deletes the policy of the API called name, with its ci.policy.delete row. A policy of
// the configuration is index.ErrConflict.
func (s *Service) Delete(ctx context.Context, name string, o auth.Origin) error {
	if err := s.readOnly(name); err != nil {
		return err
	}
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		old, err := s.row(ctx, w, name)
		if err != nil {
			return err
		}
		if err := w.DeleteCIPolicy(ctx, old.ID); err != nil {
			return err
		}
		return s.record(ctx, w, ActionPolicyDelete, *old, nil, o)
	})
	return s.txError("delete", name, err)
}

// check applies the rules of a policy of the configuration to p (config.CheckCIPolicy), and
// refuses one that writes: a policy of the API reads only.
func (s *Service) check(p Policy) error {
	cp := toConfig(p)
	if errs := config.CheckCIPolicy(s.Config.Issuers, cp); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return &serviceError{msg: strings.Join(msgs, "; "), err: auth.ErrInvalid}
	}
	if writes(cp) {
		return &serviceError{msg: "grants: a policy of the API grants reader only: write the policy in the configuration",
			err: auth.ErrScope}
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

// record appends the audit row of a change of the policy r on w, in its transaction: by the
// principal of ctx, subject the name, details the policy, the previous one of a change, and the
// origin.
func (s *Service) record(ctx context.Context, w index.Writer, action string, r index.CIPolicy, previous *index.CIPolicy, o auth.Origin) error {
	details := map[string]any{"issuer": r.Issuer, "claims": r.Claims, "grants": r.Grants}
	if previous != nil {
		details["previous"] = map[string]any{"issuer": previous.Issuer, "claims": previous.Claims, "grants": previous.Grants}
	}
	for k, v := range map[string]string{"source": o.Source, "requestId": o.RequestID, "host": o.Host} {
		if v != "" {
			details[k] = v
		}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("ci: audit details of %s: %w", action, err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return w.AppendAudit(ctx, &index.AuditEvent{At: now(), Principal: auth.PrincipalFrom(ctx).String(), Action: action,
		Subject: r.Name, IP: o.IP, Details: raw})
}

// fromConfig is the policy p of the configuration as the API shows it.
func fromConfig(p config.CIPolicy) Policy {
	pol := Policy{Name: p.Name, Issuer: p.Issuer, Claims: map[string][]string{}, Grants: []index.CIGrant{}, Source: SourceConfig}
	for _, c := range slices.Sorted(maps.Keys(p.Claims)) {
		pol.Claims[c] = slices.Clone(p.Claims[c])
	}
	for _, g := range p.Grants {
		pol.Grants = append(pol.Grants, index.CIGrant{Role: g.Role, Mask: g.Mask})
	}
	return pol
}

// fromRow is the row r as the API shows it.
func fromRow(r index.CIPolicy) Policy {
	created, updated := r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	return Policy{Name: r.Name, Issuer: r.Issuer, Claims: r.Claims, Grants: r.Grants, Source: SourceAPI, CreatedBy: r.CreatedBy,
		CreatedAt: &created, UpdatedAt: &updated}
}

// toConfig is p as a policy of the configuration, for its rules.
func toConfig(p Policy) config.CIPolicy {
	return configPolicy(index.CIPolicy{Name: p.Name, Issuer: p.Issuer, Claims: p.Claims, Grants: p.Grants})
}
