package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/policy"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// Rules is the service behind the policy rules (docs/spec/policy.md §1, §7): POST, DELETE and GET
// /api/v1/policy/rules, and lajstrom admin policy.
type Rules struct {
	// Index is the index database; required.
	Index index.Store
	// Formats reports whether a format plugin of that name is compiled in; nil accepts every name.
	Formats func(name string) bool
	// Order is the order of a format's versions, for the ranges of a rule (policy.Options.Order).
	Order func(format string) policy.VersionOrder
	// Audit records the events; nil means each goes to the index in the transaction of its change.
	Audit audit.Writer
	// Webhooks writes the events policy.rule.created and policy.rule.deleted; nil writes none.
	Webhooks *webhook.Outbox
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
}

// Rule is a policy rule as the API answers it (docs/api.md, Policy).
type Rule struct {
	ID        int64      `json:"id"`
	RepoMask  string     `json:"repoMask"`
	Name      string     `json:"name"`
	Version   string     `json:"version"`
	Variant   string     `json:"variant"`
	Effect    string     `json:"effect"`
	Reason    string     `json:"reason"`
	CreatedBy string     `json:"createdBy"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
	Expired   bool       `json:"expired"`
}

// RuleList is the answer of GET /api/v1/policy/rules.
type RuleList struct {
	Rules []Rule `json:"rules"`
}

// RuleRequest is the body of POST /api/v1/policy/rules; Version, Variant and Effect are "*", "*"
// and deny when absent.
type RuleRequest struct {
	RepoMask  string     `json:"repoMask"`
	Name      string     `json:"name"`
	Version   *string    `json:"version"`
	Variant   *string    `json:"variant"`
	Effect    string     `json:"effect"`
	Reason    string     `json:"reason"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

func (s *Rules) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ruleOf is the Rule of r at now.
func ruleOf(r index.PolicyRule, now time.Time) Rule {
	return Rule{ID: r.ID, RepoMask: r.RepoMask, Name: r.Name, Version: r.Version, Variant: r.Variant, Effect: r.Effect,
		Reason: r.Reason, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, Expired: r.Expired(now)}
}

// List returns every rule by id; with repo, the rules whose mask matches it.
func (s *Rules) List(ctx context.Context, repo *index.Repo) ([]Rule, error) {
	rows, err := s.Index.ListPolicyRules(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := []Rule{}
	for _, r := range rows {
		if repo == nil || auth.MaskMatches(r.RepoMask, repo.Format, repo.Name) {
			out = append(out, ruleOf(r, now))
		}
	}
	return out, nil
}

// Create checks req (policy.ParseRule) and creates the rule by principal, with its audit row
// policy.rule.create, its event policy.rule.created and the invalidation of the documents of the
// repos its mask matches, in one transaction.
func (s *Rules) Create(ctx context.Context, req RuleRequest, principal string, o Origin) (*Rule, error) {
	row := index.PolicyRule{RepoMask: req.RepoMask, Name: req.Name, Version: "*", Variant: "*", Effect: cmp.Or(req.Effect, index.PolicyDeny),
		Reason: req.Reason, CreatedBy: principal, CreatedAt: s.now().UTC(), ExpiresAt: req.ExpiresAt}
	if req.Version != nil {
		row.Version = strings.TrimSpace(*req.Version)
	}
	if req.Variant != nil {
		row.Variant = *req.Variant
	}
	if err := policy.ParseRule(row, s.now(), s.Order, s.Formats); err != nil {
		return nil, invalid("%s", err.Error())
	}
	var ev index.AuditEvent
	err := s.Index.Tx(ctx, func(w index.Writer) (err error) {
		if err := w.CreatePolicyRule(ctx, &row); err != nil {
			return err
		}
		if ev, err = s.changed(ctx, w, policy.ActionRuleCreate, webhook.TypePolicyRuleCreated, row, principal, o); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("api: create the rule over %s: %w", row.RepoMask, err)
	}
	s.writeAudit(ctx, ev)
	r := ruleOf(row, s.now())
	return &r, nil
}

// Delete deletes the rule id by principal, as Create creates one; index.ErrNotFound when there is
// none. may decides, before anything changes, whether the caller may delete a rule of its mask.
func (s *Rules) Delete(ctx context.Context, id int64, principal string, o Origin, may func(mask string) error) error {
	var ev index.AuditEvent
	err := s.Index.Tx(ctx, func(w index.Writer) (err error) {
		row, err := w.GetPolicyRule(ctx, id)
		if err != nil {
			return err
		}
		if may != nil {
			if err := may(row.RepoMask); err != nil {
				return err
			}
		}
		if err := w.DeletePolicyRule(ctx, id); err != nil {
			return err
		}
		ev, err = s.changed(ctx, w, policy.ActionRuleDelete, webhook.TypePolicyRuleDeleted, *row, principal, o)
		return err
	})
	if err != nil {
		return err
	}
	s.writeAudit(ctx, ev)
	return nil
}

// changed writes on w what a change of the rule row does beside it: the documents of the repos
// its mask matches invalidated, its audit row of action and its event of typ.
func (s *Rules) changed(ctx context.Context, w index.Writer, action, typ string, row index.PolicyRule, principal string, o Origin,
) (index.AuditEvent, error) {
	if err := policy.Invalidate(ctx, w, row.RepoMask); err != nil {
		return index.AuditEvent{}, err
	}
	details := map[string]any{"repoMask": row.RepoMask, "name": row.Name, "version": row.Version, "variant": row.Variant,
		"effect": row.Effect, "reason": row.Reason}
	if row.ExpiresAt != nil {
		details["expiresAt"] = row.ExpiresAt.UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(o.AddTo(auth.AddSignIn(ctx, details)))
	if err != nil {
		return index.AuditEvent{}, err
	}
	ev := index.AuditEvent{At: s.now(), Principal: principal, Action: action, Repo: row.RepoMask,
		Subject: "rule:" + strconv.FormatInt(row.ID, 10), IP: o.IP, Details: raw, State: index.AuditOK}
	if s.Audit == nil {
		if err := w.AppendAudit(ctx, &ev); err != nil {
			return index.AuditEvent{}, err
		}
	}
	f, n, _ := auth.ParseMask(row.RepoMask)
	data := webhook.RuleData{Common: webhook.CommonOf(ev), Rule: webhook.RuleOf(row)}
	err = s.Webhooks.Enqueue(ctx, w, webhook.Event{Type: typ, Repo: index.Repo{Format: f, Name: n}, Subject: ev.Subject, Data: data,
		Mask: row.RepoMask})
	return ev, err
}

// writeAudit writes ev through the audit.Writer after the commit, when there is one.
func (s *Rules) writeAudit(ctx context.Context, ev index.AuditEvent) {
	if s.Audit == nil {
		return
	}
	if err := s.Audit.Write(ctx, ev); err != nil {
		cmp.Or(s.Log, slog.Default()).ErrorContext(ctx, "api: audit row of a policy rule not written", "action", ev.Action,
			"subject", ev.Subject, "error", err.Error())
	}
}

// mayRule decides whether the request's principal may create or delete a rule of mask: manage on
// the repo of a mask that names one, the admin role (policy, rbac.md §2) for any other.
func (h *handler) mayRule(r *http.Request, mask string) error {
	if f, n, err := auth.ParseMask(mask); err == nil && !strings.Contains(mask, "*") {
		return h.allow(r, auth.ActionManage, f, n)
	}
	return h.allow(r, auth.ActionManage, auth.System, auth.System)
}

// listRules answers GET /api/v1/policy/rules: every rule for the admin role; with
// ?repo=<format>/<repo>, the rules whose mask matches it, for whoever may read the repo.
func (h *handler) listRules(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if h.rules == nil {
		return 0, nil, &clientError{msg: "policy rules are not enabled in this server", err: format.ErrUnsupported}
	}
	ref := r.URL.Query().Get("repo")
	if ref == "" {
		if err := h.allow(r, auth.ActionManage, auth.System, auth.System); err != nil {
			return 0, nil, err
		}
		rules, err := h.rules.List(r.Context(), nil)
		return http.StatusOK, RuleList{Rules: rules}, err
	}
	f, n, ok := strings.Cut(ref, "/")
	if !ok {
		return 0, nil, invalid("repo: %q is not <format>/<repo>", ref)
	}
	if err := h.allow(r, auth.ActionRead, f, n); err != nil {
		return 0, nil, err
	}
	repo, err := h.repos.Index.GetRepo(r.Context(), f, n)
	if err != nil {
		return 0, nil, repoError("list the rules of", f, n, err)
	}
	rules, err := h.rules.List(r.Context(), repo)
	return http.StatusOK, RuleList{Rules: rules}, err
}

// createRule answers POST /api/v1/policy/rules: 201 with the rule.
func (h *handler) createRule(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if h.rules == nil {
		return 0, nil, &clientError{msg: "policy rules are not enabled in this server", err: format.ErrUnsupported}
	}
	var req RuleRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	if _, _, err := auth.ParseMask(req.RepoMask); err != nil {
		return 0, nil, invalid("repoMask: %s", err.Error())
	}
	if err := h.mayRule(r, req.RepoMask); err != nil {
		return 0, nil, err
	}
	rule, err := h.rules.Create(r.Context(), req, auth.PrincipalFrom(r.Context()).String(), origin(r))
	if err != nil {
		return 0, nil, err
	}
	w.Header().Set("Location", "/api/v1/policy/rules/"+strconv.FormatInt(rule.ID, 10))
	return http.StatusCreated, rule, nil
}

// deleteRule answers DELETE /api/v1/policy/rules/{id}: 204, 404 for a rule that does not exist or
// that the caller may not delete.
func (h *handler) deleteRule(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if h.rules == nil {
		return 0, nil, &clientError{msg: "policy rules are not enabled in this server", err: format.ErrUnsupported}
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, nil, &clientError{msg: fmt.Sprintf("no policy rule %q", r.PathValue("id")), err: index.ErrNotFound}
	}
	err = h.rules.Delete(r.Context(), id, auth.PrincipalFrom(r.Context()).String(), origin(r), func(mask string) error {
		if err := h.mayRule(r, mask); err != nil {
			return &clientError{msg: fmt.Sprintf("no policy rule %d", id), err: index.ErrNotFound}
		}
		return nil
	})
	if errors.Is(err, index.ErrNotFound) {
		return 0, nil, &clientError{msg: fmt.Sprintf("no policy rule %d", id), err: index.ErrNotFound}
	}
	if err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}
