package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The types of events (docs/spec/webhooks.md §1), as a webhook's events name them. The
// CloudEvents type is typePrefix and the type.
const (
	TypeVersionPublished = "version.published"
	TypeVersionDeleted   = "version.deleted"
	TypeRepoCreated      = "repo.created"
	TypeRepoDeleted      = "repo.deleted"
	// The events of the policy (#101; docs/spec/policy.md §7).
	TypePolicyRuleCreated    = "policy.rule.created"
	TypePolicyRuleDeleted    = "policy.rule.deleted"
	TypePolicyVerdictCreated = "policy.verdict.created"
	TypePolicyVerdictDeleted = "policy.verdict.deleted"
	// The events of a CI trust policy of the API (#937; docs/spec/rbac.md §11).
	TypeCIPolicyCreated  = "ci.policy.created"
	TypeCIPolicyUpdated  = "ci.policy.updated"
	TypeCIPolicyRenewed  = "ci.policy.renewed"
	TypeCIPolicyApproved = "ci.policy.approved"
	TypeCIPolicyExpired  = "ci.policy.expired"
	TypeCIPolicyDeleted  = "ci.policy.deleted"
	// The events of a request for a second admin (#1336; docs/spec/rbac.md §13).
	TypeApprovalRequested = "approval.requested"
	TypeApprovalApproved  = "approval.approved"
	TypeApprovalRejected  = "approval.rejected"
	TypeApprovalExpired   = "approval.expired"
	// TypePing is sent only by the test of one webhook; no webhook asks for it.
	TypePing = "webhook.ping"
)

// Types are the types a webhook may ask for, in the order of webhooks.md §1.
var Types = []string{TypeVersionPublished, TypeVersionDeleted, TypeRepoCreated, TypeRepoDeleted,
	TypePolicyRuleCreated, TypePolicyRuleDeleted, TypePolicyVerdictCreated, TypePolicyVerdictDeleted,
	TypeCIPolicyCreated, TypeCIPolicyUpdated, TypeCIPolicyRenewed, TypeCIPolicyApproved, TypeCIPolicyExpired, TypeCIPolicyDeleted,
	TypeApprovalRequested, TypeApprovalApproved, TypeApprovalRejected, TypeApprovalExpired}

// typePrefix makes a type a CloudEvents type: the reverse domain name of lajstrom.dev.
const typePrefix = "dev.lajstrom."

// Schema is the version of the data of an event (webhooks.md §3).
const Schema = 1

// Event is what a write knows of its event: the type, the repo it is of, the subject of its
// audit row, and its data.
type Event struct {
	Type    string
	Repo    index.Repo // Format, Name and Kind are read
	Subject string     // the audit row's subject; "" for a repo
	Data    any        // VersionData, RepoData, RuleData, VerdictData, CIPolicyData, ApprovalData or PingData, marshalled as it is
	// Mask is the repo mask of a policy rule's event, which goes to the webhooks whose mask
	// covers it (Covers) instead of those whose mask matches Repo; its source is the rules'.
	Mask string
	// Masks are the masks of an event of the admin, those of the grants of a CI trust policy
	// before and after its change, or the mask of a request's change (ApprovalEvent): it goes to
	// the webhooks of the admin, whose mask has a *, that cover one of them (Covers), instead of
	// those whose mask matches Repo; its source is the policies', or route.
	Masks []string
	// route is the path of the source of an event that is no repo's, rule's or CI trust
	// policy's: a request's, /api/v1/approvals.
	route string
}

// Actor is the principal of a write and a user's name at the time.
type Actor struct {
	Principal string `json:"principal"`
	Name      string `json:"name,omitempty"`
}

// RepoRef names a repo in an event.
type RepoRef struct {
	Format string `json:"format"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
}

// RefOf is the RepoRef of r.
func RefOf(r index.Repo) RepoRef { return RepoRef{Format: r.Format, Name: r.Name, Kind: r.Kind} }

// File is a file layer of a published version.
type File struct {
	Name      string        `json:"name"`
	MediaType string        `json:"mediaType"`
	Digest    digest.Digest `json:"digest"`
	Size      int64         `json:"size"`
}

// Common are the fields of every event (webhooks.md §3).
type Common struct {
	Schema    int    `json:"schema"`
	Action    string `json:"action"`
	Actor     Actor  `json:"actor"`
	Audit     int64  `json:"audit,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// CommonOf returns the common fields of the event of the audit row ev: its action, its
// principal and the principal's name, its id, and the request id of its details.
func CommonOf(ev index.AuditEvent) Common {
	c := Common{Schema: Schema, Action: ev.Action, Actor: Actor{Principal: ev.Principal, Name: ev.PrincipalName}, Audit: ev.ID}
	var details struct {
		RequestID string `json:"requestId"`
	}
	_ = json.Unmarshal(ev.Details, &details) // details the writer marshalled; nil is none
	c.RequestID = details.RequestID
	return c
}

// VersionData is the data of a version.published or version.deleted.
type VersionData struct {
	Common
	Repo          RepoRef       `json:"repo"`
	Package       string        `json:"package"`
	Version       string        `json:"version"`
	Variant       string        `json:"variant"`
	Tag           string        `json:"tag"`
	Digest        digest.Digest `json:"digest"`
	ArtifactType  string        `json:"artifactType"`
	OCIRepository string        `json:"ociRepository"`
	Files         []File        `json:"files,omitempty"`
	Upstream      string        `json:"upstream,omitempty"`
}

// RepoData is the data of a repo.created or repo.deleted; Versions and Force only for a delete.
type RepoData struct {
	Common
	Repo     RepoRef `json:"repo"`
	Versions *int    `json:"versions,omitempty"`
	Force    *bool   `json:"force,omitempty"`
}

// RuleData is the data of a policy.rule.created or policy.rule.deleted.
type RuleData struct {
	Common
	Rule Rule `json:"rule"`
}

// Rule is a policy rule in an event.
type Rule struct {
	ID        int64      `json:"id"`
	RepoMask  string     `json:"repoMask"`
	Name      string     `json:"name"`
	Version   string     `json:"version"`
	Variant   string     `json:"variant"`
	Effect    string     `json:"effect"`
	Reason    string     `json:"reason"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// RuleOf is the Rule of r.
func RuleOf(r index.PolicyRule) Rule {
	return Rule{ID: r.ID, RepoMask: r.RepoMask, Name: r.Name, Version: r.Version, Variant: r.Variant, Effect: r.Effect, Reason: r.Reason,
		ExpiresAt: r.ExpiresAt}
}

// VerdictData is the data of a policy.verdict.created or policy.verdict.deleted: the version's
// coordinates, as a version's event has them where the subject is one, and the verdict.
type VerdictData struct {
	Common
	Repo          RepoRef       `json:"repo"`
	Package       string        `json:"package"`
	Version       string        `json:"version,omitempty"`
	Variant       string        `json:"variant,omitempty"`
	OCIRepository string        `json:"ociRepository"`
	Digest        digest.Digest `json:"digest"`
	Verdict       Verdict       `json:"verdict"`
}

// Verdict is a verdict in an event.
type Verdict struct {
	Verdict     string        `json:"verdict"`
	Reason      string        `json:"reason"`
	Source      string        `json:"source"`
	Scope       string        `json:"scope"`
	ValidUntil  *time.Time    `json:"validUntil"`
	Attestation digest.Digest `json:"attestation"`
}

// CIPolicyData is the data of the event of a CI trust policy of the API: ci.policy.created,
// .updated, .renewed, .approved, .expired or .deleted (docs/spec/webhooks.md §3).
type CIPolicyData struct {
	Common
	Policy CIPolicy `json:"policy"`
}

// CIPolicy is a CI trust policy in an event, as the change left it; a deleted one as it was.
type CIPolicy struct {
	Name      string              `json:"name"`
	Issuer    string              `json:"issuer"`
	Claims    map[string][]string `json:"claims"`
	Grants    []index.CIGrant     `json:"grants"`
	State     string              `json:"state"`
	ExpiresAt *time.Time          `json:"expiresAt"`
	// RenewalExpiresAt is the expiry of a renewal that waits for an admin's approval.
	RenewalExpiresAt *time.Time `json:"renewalExpiresAt,omitempty"`
}

// CIPolicyOf is the CIPolicy of r.
func CIPolicyOf(r index.CIPolicy) CIPolicy {
	return CIPolicy{Name: r.Name, Issuer: r.Issuer, Claims: r.Claims, Grants: r.Grants, State: r.State, ExpiresAt: r.ExpiresAt,
		RenewalExpiresAt: r.RenewalExpiresAt}
}

// PingData is the data of a webhook.ping.
type PingData struct {
	Common
	Webhook struct {
		ID   int64  `json:"id"`
		Mask string `json:"mask"`
	} `json:"webhook"`
}

// cloudEvent is the document of webhooks.md §2, the CloudEvents 1.0 structured mode.
type cloudEvent struct {
	SpecVersion     string    `json:"specversion"`
	ID              string    `json:"id"`
	Source          string    `json:"source"`
	Type            string    `json:"type"`
	Subject         string    `json:"subject,omitempty"`
	Time            time.Time `json:"time"`
	DataContentType string    `json:"datacontenttype"`
	Data            any       `json:"data"`
}

// Outbox writes the deliveries of events in the transaction of their audit rows
// (webhooks.md §4). A nil *Outbox writes nothing. Its methods may be called concurrently.
type Outbox struct {
	// Source is server.externalURL, the base of every event's source; "" on the dev stack.
	Source string
	// Now is the clock of an event's time and id; nil means time.Now.
	Now func() time.Time
}

// Enqueue writes on w one delivery of ev per active webhook whose mask matches ev.Repo (or covers
// ev.Mask, or, of the admin, one of ev.Masks) and whose events include ev.Type, each with the same
// body and event id; nothing when none matches.
func (o *Outbox) Enqueue(ctx context.Context, w index.Writer, ev Event) error {
	if o == nil {
		return nil
	}
	matched, err := matching(ctx, w, ev)
	if err != nil || len(matched) == 0 {
		return err
	}
	return o.write(ctx, w, ev, matched)
}

// matching returns the active webhooks, read on r, that get ev: whose mask it goes to (goesTo) and
// whose events include its type.
func matching(ctx context.Context, r index.Reader, ev Event) ([]index.Webhook, error) {
	hooks, err := r.ListWebhooks(ctx)
	if err != nil {
		return nil, fmt.Errorf("webhook: the webhooks of a %s: %w", ev.Type, err)
	}
	var matched []index.Webhook
	for _, h := range hooks {
		if h.Active && ev.goesTo(h.Mask) && Wants(h.Events, ev.Type) {
			matched = append(matched, h)
		}
	}
	return matched, nil
}

// write writes on w one delivery of ev to each of hooks, each with the same body and event id.
func (o *Outbox) write(ctx context.Context, w index.Writer, ev Event, hooks []index.Webhook) error {
	at := o.now()
	source := o.Source + "/" + ev.Repo.Format + "/" + ev.Repo.Name
	switch {
	case ev.route != "":
		source = o.Source + ev.route
	case ev.Mask != "":
		source = o.Source + "/api/v1/policy/rules"
	case ev.Masks != nil:
		source = o.Source + "/api/v1/ci/policies"
	}
	id, body, err := o.document(ev, at, source)
	if err != nil {
		return err
	}
	ds := make([]index.WebhookDelivery, len(hooks))
	for i, h := range hooks {
		ds[i] = index.WebhookDelivery{WebhookID: h.ID, EventID: id, EventType: ev.Type, Payload: body, CreatedAt: at}
	}
	if err := w.CreateWebhookDeliveries(ctx, ds); err != nil {
		return fmt.Errorf("webhook: the deliveries of a %s: %w", ev.Type, err)
	}
	return nil
}

// goesTo reports whether ev goes to a webhook of mask, whatever its events: one that matches its
// repo, that covers the mask of a policy rule's, or, of the admin, one of the masks of a CI trust
// policy's.
func (ev Event) goesTo(mask string) bool {
	switch {
	case ev.Mask != "":
		return Covers(mask, ev.Mask)
	case ev.Masks != nil:
		return strings.Contains(mask, "*") && slices.ContainsFunc(ev.Masks, func(m string) bool { return Covers(mask, m) })
	}
	return Matches(mask, ev.Repo.Format, ev.Repo.Name)
}

// document returns the id and the body of ev at at, from source.
func (o *Outbox) document(ev Event, at time.Time, source string) (string, []byte, error) {
	id := index.NewUID(at)
	body, err := json.Marshal(cloudEvent{SpecVersion: "1.0", ID: id, Source: source, Type: typePrefix + ev.Type,
		Subject: ev.Subject, Time: at.UTC(), DataContentType: "application/json", Data: ev.Data})
	if err != nil {
		return "", nil, fmt.Errorf("webhook: the body of a %s: %w", ev.Type, err)
	}
	return id, body, nil
}

func (o *Outbox) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Matches reports whether the repo mask (docs/spec/rbac.md §3) matches formatName/repo. A mask
// that does not parse matches nothing.
func Matches(mask, formatName, repo string) bool {
	f, n, err := auth.ParseMask(mask)
	return err == nil && (f == "*" || f == formatName) && (n == "*" || n == repo)
}

// Covers reports whether a webhook's mask covers mask, a policy rule's: every repo mask matches,
// the webhook's matches too, segment by segment (docs/spec/webhooks.md §3).
func Covers(mask, of string) bool {
	f, n, err := auth.ParseMask(mask)
	of1, on, err2 := auth.ParseMask(of)
	return err == nil && err2 == nil && (f == "*" || f == of1) && (n == "*" || n == on)
}

// Wants reports whether a webhook with events gets an event of type typ: none means every type.
func Wants(events []string, typ string) bool {
	return len(events) == 0 || slices.Contains(events, typ)
}

// IsType reports whether typ is a type a webhook may ask for.
func IsType(typ string) bool { return slices.Contains(Types, typ) }

// CloudEventType is the CloudEvents type of typ.
func CloudEventType(typ string) string { return typePrefix + strings.TrimPrefix(typ, typePrefix) }
