package policy

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The effects of a decision (docs/spec/policy.md §3).
const (
	EffectAllow      = index.PolicyAllow
	EffectDeny       = index.PolicyDeny
	EffectQuarantine = index.PolicyQuarantine
)

// PredicateType is the in-toto predicate type of a verdict (docs/spec/manifest-profile.md §6.4).
const PredicateType = attest.VerdictPredicateType

// The sources of the verdicts the server makes (docs/spec/policy.md §2).
const (
	SourceManual     = "lajstrom:manual"
	SourceQuarantine = "lajstrom:quarantine"
)

// ErrDenied is wrapped by a refusal (*DeniedError): format.StatusOf answers it 403, the facade
// DENIED.
var ErrDenied = errors.New("policy: denied")

// Subject is what a decision is about (docs/spec/policy.md §3).
type Subject struct {
	// Repo is the repo that holds or would hold the artifact: a group's member, never the group.
	Repo index.Repo
	// Name is the package name as lajstrom.name spells it; an oci repo's path below the repo.
	Name string
	// Version and Variant are the coordinates; an oci repo's tag, "" for a digest, and "".
	Version, Variant string
	// Digest is the manifest's; "" for a coordinate the repo does not hold.
	Digest digest.Digest
	// Published is the quarantine's clock as the caller knows it (policy.md §4): a held
	// version's created_at, an upstream entry's publish time; zero for none. The Decider takes
	// the earlier policy_seen row when the repo has one.
	Published time.Time
}

// String is "<name> <version>[ <variant>]", the subject of an audit row.
func (s Subject) String() string {
	out := s.Name
	if s.Version != "" {
		out += " " + s.Version
	}
	if s.Variant != "" {
		out += " " + s.Variant
	}
	return out
}

// Verdict is a decision (docs/spec/policy.md §3).
type Verdict struct {
	// Effect is EffectAllow, EffectDeny or EffectQuarantine.
	Effect string
	// Reason is the rule's or the verdict's reason; "" for an allow and for a quarantine of the
	// repo's own.
	Reason string
	// Rule is the id of the rule that decided; 0 when none did.
	Rule int64
	// Attestation is the referrer of the verdict that decided; "" when none did.
	Attestation digest.Digest
	// Source is that verdict's source.
	Source string
	// Lifts is the end of a quarantine; zero for another effect.
	Lifts time.Time
	// Until is when the decision may change by the clock alone; zero for never.
	Until time.Time
}

// Allowed reports whether the decision lets the artifact through.
func (v Verdict) Allowed() bool { return v.Effect == "" || v.Effect == EffectAllow }

// DeniedError is a refusal of Subject by Verdict; it wraps ErrDenied.
type DeniedError struct {
	Subject Subject
	Verdict Verdict
}

// Error is the message of docs/spec/policy.md §6.6: the name and the version, without the
// variant, which the audit row has.
func (e *DeniedError) Error() string {
	what := e.Subject.Name
	if e.Subject.Version != "" {
		what += " " + e.Subject.Version
	}
	if e.Verdict.Effect == EffectQuarantine {
		msg := what + " is quarantined"
		if !e.Verdict.Lifts.IsZero() {
			msg += " until " + e.Verdict.Lifts.UTC().Format(time.RFC3339)
		}
		if e.Verdict.Reason != "" {
			msg += ": " + e.Verdict.Reason
		}
		return msg
	}
	return fmt.Sprintf("%s is denied by policy: %s", what, e.Verdict.Reason)
}

// Unwrap is ErrDenied.
func (e *DeniedError) Unwrap() error { return ErrDenied }

// Refusal returns the refusal of s by v, nil when v allows.
func Refusal(s Subject, v Verdict) error {
	if v.Allowed() {
		return nil
	}
	return &DeniedError{Subject: s, Verdict: v}
}

// Options configures a Decider.
type Options struct {
	// Index is the index database; required.
	Index index.Store
	// Order returns the order of a format's versions (format.VersionComparer), nil for a format
	// without one; nil means no format orders its versions.
	Order func(format string) VersionOrder
	// Audit writes the rows policy.refuse; nil means Index.AppendAudit.
	Audit audit.Writer
	// Origin returns the client's address and the request id of ctx, for an audit row
	// (server.ClientAddr, server.RequestID); nil means none.
	Origin func(ctx context.Context) (addr, requestID string)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
}

// Decider takes the decisions of docs/spec/policy.md §3 over the rules, which it keeps while
// policy_state.generation stays, and the verdicts of the index. A nil *Decider allows
// everything, so that a caller without policy needs no check.
type Decider struct {
	o Options

	mu    sync.Mutex
	gen   int64
	rules []index.PolicyRule
}

// New returns a Decider.
func New(o Options) *Decider {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Decider{o: o, gen: -1}
}

// Now is the Decider's clock.
func (d *Decider) Now() time.Time { return d.o.Now() }

// Decide decides s: the rules, the groups of its repo when a rule's mask may name one, the
// verdicts of its digest and, for a quarantine, the repo's policy_seen row, read from the index.
func (d *Decider) Decide(ctx context.Context, s Subject) (Verdict, error) {
	if d == nil {
		return Verdict{Effect: EffectAllow}, nil
	}
	return d.decide(ctx, d.o.Index, s)
}

// Check is Decide as an error: a *DeniedError when s is refused.
func (d *Decider) Check(ctx context.Context, s Subject) error {
	v, err := d.Decide(ctx, s)
	if err != nil {
		return err
	}
	return Refusal(s, v)
}

// decide is Decide through r.
func (d *Decider) decide(ctx context.Context, r index.Reader, s Subject) (Verdict, error) {
	rules, err := d.Rules(ctx, r)
	if err != nil {
		return Verdict{}, err
	}
	groups, err := groupsFor(ctx, r, s.Repo, rules)
	if err != nil {
		return Verdict{}, err
	}
	var verdicts []index.Verdict
	if s.Digest != "" {
		if verdicts, err = r.ListVerdicts(ctx, index.VerdictFilter{Subject: s.Digest}); err != nil {
			return Verdict{}, fmt.Errorf("policy: the verdicts of %s: %w", s.Digest, err)
		}
	}
	e := evaluation{now: d.o.Now(), subject: s, rules: rules, groups: groups, verdicts: verdicts, order: d.order(s.Repo.Format)}
	e.seen = func() (time.Time, bool) { return seen(ctx, r, s) }
	return e.decide(), nil
}

// Rules returns every rule, read again through r when policy_state.generation moved.
func (d *Decider) Rules(ctx context.Context, r index.Reader) ([]index.PolicyRule, error) {
	gen, err := r.PolicyGeneration(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy: the generation: %w", err)
	}
	d.mu.Lock()
	if gen == d.gen {
		rules := d.rules
		d.mu.Unlock()
		return rules, nil
	}
	d.mu.Unlock()
	rules, err := r.ListPolicyRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy: the rules: %w", err)
	}
	d.mu.Lock()
	if gen >= d.gen {
		d.gen, d.rules = gen, rules
	}
	d.mu.Unlock()
	return rules, nil
}

// order is the order of the versions of formatName, nil when it has none.
func (d *Decider) order(formatName string) VersionOrder {
	if d.o.Order == nil {
		return nil
	}
	return d.o.Order(formatName)
}

// groupsFor returns the groups repo is a member of, read only when a rule has a mask without a
// wildcard that is not the repo's own: only such a mask can name a group of it.
func groupsFor(ctx context.Context, r index.Reader, repo index.Repo, rules []index.PolicyRule) ([]string, error) {
	need := false
	for _, rule := range rules {
		if !strings.Contains(rule.RepoMask, "*") && rule.RepoMask != repo.Format+"/"+repo.Name {
			need = true
			break
		}
	}
	if !need || repo.ID == 0 {
		return nil, nil
	}
	gs, err := r.ListGroupsOf(ctx, repo.ID)
	if err != nil {
		return nil, fmt.Errorf("policy: the groups of %s/%s: %w", repo.Format, repo.Name, err)
	}
	names := make([]string, 0, len(gs))
	for _, g := range gs {
		names = append(names, g.Format+"/"+g.Name)
	}
	return names, nil
}

// seen returns the policy_seen row of s, when its repo has one.
func seen(ctx context.Context, r index.Reader, s Subject) (time.Time, bool) {
	row, err := r.GetPolicySeen(ctx, s.Repo.ID, s.Name, s.Version, s.Variant)
	if err != nil {
		return time.Time{}, false
	}
	return row.SeenAt, true
}

// evaluation is one decision's inputs.
type evaluation struct {
	now      time.Time
	subject  Subject
	rules    []index.PolicyRule
	groups   []string // the groups of the subject's repo, <format>/<name>
	verdicts []index.Verdict
	order    VersionOrder
	// seen returns the policy_seen row of the subject, asked only for a quarantine.
	seen func() (time.Time, bool)
}

// found is what an evaluation read that holds: the first verdict in force of each kind, the
// first rule of each effect, and the earliest expiry among them.
type found struct {
	deny, quarantine, allow *index.Verdict
	allowRule, denyRule     *index.PolicyRule
	until                   time.Time
}

// expires lowers f.until to t when t is after now and before it.
func (f *found) expires(now time.Time, t *time.Time) {
	if t != nil && t.After(now) && (f.until.IsZero() || t.Before(f.until)) {
		f.until = *t
	}
}

// collect reads the verdicts and the rules that hold for the subject.
func (e *evaluation) collect() found {
	var f found
	for i := range e.verdicts {
		v := &e.verdicts[i]
		if !e.inForce(v) {
			continue
		}
		f.expires(e.now, v.ValidUntil)
		switch v.Verdict {
		case index.PolicyDeny:
			f.deny = cmp.Or(f.deny, v)
		case index.PolicyQuarantine:
			f.quarantine = cmp.Or(f.quarantine, v)
		case index.PolicyAllow:
			f.allow = cmp.Or(f.allow, v)
		}
	}
	for i := range e.rules {
		r := &e.rules[i]
		if !e.holds(r) {
			continue
		}
		f.expires(e.now, r.ExpiresAt)
		switch r.Effect {
		case index.PolicyAllow:
			f.allowRule = cmp.Or(f.allowRule, r)
		case index.PolicyDeny:
			f.denyRule = cmp.Or(f.denyRule, r)
		}
	}
	return f
}

// decide is docs/spec/policy.md §3.
func (e *evaluation) decide() Verdict {
	f := e.collect()
	switch {
	case f.deny != nil:
		return Verdict{Effect: EffectDeny, Reason: f.deny.Reason, Attestation: f.deny.Referrer, Source: f.deny.Source, Until: f.until}
	case f.allowRule != nil:
		return Verdict{Effect: EffectAllow, Rule: f.allowRule.ID, Until: f.until}
	case f.denyRule != nil:
		return Verdict{Effect: EffectDeny, Reason: f.denyRule.Reason, Rule: f.denyRule.ID, Until: f.until}
	}
	return e.quarantined(&f)
}

// quarantined is step 4 of the decision: a quarantine verdict, or the repo's quarantine, unless an
// allow verdict releases it.
func (e *evaluation) quarantined(f *found) Verdict {
	lifts := e.quarantineEnd()
	f.expires(e.now, &lifts)
	q := f.quarantine
	switch {
	case f.allow != nil, q == nil && lifts.IsZero():
		return Verdict{Effect: EffectAllow, Until: f.until}
	case q == nil:
		return Verdict{Effect: EffectQuarantine, Lifts: lifts, Until: f.until}
	}
	v := Verdict{Effect: EffectQuarantine, Reason: q.Reason, Attestation: q.Referrer, Source: q.Source, Lifts: lifts, Until: f.until}
	if q.ValidUntil != nil && (lifts.IsZero() || q.ValidUntil.After(lifts)) {
		v.Lifts = *q.ValidUntil
	}
	return v
}

// inForce reports whether the verdict v of the subject's digest holds in its repo now: not
// expired, and its scope matches the repo (the repo that holds it when it has none).
func (e *evaluation) inForce(v *index.Verdict) bool {
	if v.Subject != e.subject.Digest || v.Expired(e.now) {
		return false
	}
	if v.Scope == "" {
		return v.RepoID == e.subject.Repo.ID
	}
	return e.maskHolds(v.Scope)
}

// holds reports whether the rule r holds for the subject (docs/spec/policy.md §1).
func (e *evaluation) holds(r *index.PolicyRule) bool {
	s := e.subject
	if r.Expired(e.now) || !e.maskHolds(r.RepoMask) || !matchName(r.Name, s.Name) {
		return false
	}
	if r.Variant != "*" && !matchName(r.Variant, s.Variant) {
		return false
	}
	c, err := parseConstraint(r.Version)
	if err != nil {
		return false // a row that ParseRule did not check holds nowhere
	}
	return c.matches(s.Version, e.order)
}

// maskHolds reports whether a repo mask matches the subject's repo, or names one of its groups
// exactly (docs/spec/policy.md §1.1).
func (e *evaluation) maskHolds(mask string) bool {
	repo := e.subject.Repo
	if auth.MaskMatches(mask, repo.Format, repo.Name) {
		return true
	}
	for _, g := range e.groups {
		if mask == g {
			return true
		}
	}
	return false
}

// quarantineEnd returns the end of the repo's quarantine of the subject when it is not over yet,
// zero otherwise (docs/spec/policy.md §4).
func (e *evaluation) quarantineEnd() time.Time {
	dur := Quarantine(e.subject.Repo)
	if dur <= 0 {
		return time.Time{}
	}
	clock := e.subject.Published
	if clock.IsZero() || clock.After(e.now) {
		clock = e.now
	}
	if !clock.Add(dur).After(e.now) {
		return time.Time{}
	}
	if e.seen != nil {
		if t, ok := e.seen(); ok && t.Before(clock) {
			clock = t
		}
	}
	if end := clock.Add(dur); end.After(e.now) {
		return end
	}
	return time.Time{}
}

// The keys of a repo's policy that this package reads (docs/spec/policy.md §9).
const (
	KeyEnforcePull = "enforcePull"
	KeyQuarantine  = "quarantineNewVersions"
)

// The bounds of quarantineNewVersions.
const (
	MinQuarantine = time.Minute
	MaxQuarantine = 8760 * time.Hour
)

// Quarantine returns policy.quarantineNewVersions of a proxy repo, 0 for none or for a repo of
// another kind. A value that does not parse is none: the API checks the policy.
func Quarantine(repo index.Repo) time.Duration {
	if repo.Kind != index.KindProxy || len(repo.Policy) == 0 {
		return 0
	}
	var p struct {
		Q string `json:"quarantineNewVersions"`
	}
	if json.Unmarshal(repo.Policy, &p) != nil || p.Q == "" {
		return 0
	}
	d, err := time.ParseDuration(p.Q)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// EnforcePull reports whether policy.enforcePull is true on repo.
func EnforcePull(repo index.Repo) bool {
	if len(repo.Policy) == 0 {
		return false
	}
	var p struct {
		E bool `json:"enforcePull"`
	}
	return json.Unmarshal(repo.Policy, &p) == nil && p.E
}

// Refuse appends the audit row policy.refuse of err (docs/spec/policy.md §6.6), the refusal of
// a request by principal on surface (SurfaceDownload, …), in a transaction of its own. A
// failure is logged: the refusal stands.
func (d *Decider) Refuse(ctx context.Context, surface string, err *DeniedError, principal auth.Principal) {
	if d == nil || err == nil {
		return
	}
	s, v := err.Subject, err.Verdict
	details := map[string]any{"surface": surface, "effect": v.Effect, "reason": v.Reason}
	if v.Effect == EffectQuarantine && !v.Lifts.IsZero() {
		details["lifts"] = v.Lifts.UTC().Format(time.RFC3339)
	}
	if v.Rule != 0 {
		details["rule"] = v.Rule
	}
	if v.Attestation != "" {
		details["verdict"], details["source"] = v.Attestation.String(), v.Source
	}
	ev := index.AuditEvent{At: d.o.Now(), Principal: principal.String(), Action: ActionRefuse, Repo: s.Repo.Format + "/" + s.Repo.Name,
		Subject: s.String(), Digest: s.Digest}
	if d.o.Origin != nil {
		addr, id := d.o.Origin(ctx)
		ev.IP = addr
		if id != "" {
			details["requestId"] = id
		}
	}
	ev.Details, _ = json.Marshal(details) // strings and integers always marshal
	var werr error
	if d.o.Audit != nil {
		werr = d.o.Audit.Write(ctx, ev)
	} else {
		werr = d.o.Index.AppendAudit(context.WithoutCancel(ctx), &ev)
	}
	if werr != nil {
		d.o.Log.ErrorContext(ctx, "policy: audit row of a refusal not written", "repo", ev.Repo, "subject", ev.Subject,
			"error", werr.Error())
	}
}

// The audit actions of policy (docs/spec/rbac.md §5).
const (
	ActionRefuse        = "policy.refuse"
	ActionRuleCreate    = "policy.rule.create"
	ActionRuleDelete    = "policy.rule.delete"
	ActionVerdictCreate = "policy.verdict.create"
	ActionVerdictDelete = "policy.verdict.delete"
)

// The surfaces of a refusal (docs/spec/policy.md §6.6).
const (
	SurfaceDownload = "download"
	SurfaceUpload   = "upload"
	SurfaceFetch    = "fetch"
	SurfacePrefetch = "prefetch"
	SurfacePull     = "pull"
	SurfacePush     = "push"
)
