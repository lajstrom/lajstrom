package auth

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The access report and its review (docs/spec/rbac.md §14, #1279): who has what at one moment,
// and the record that a person reviewed it.

// The scopes of the access report and of its review (docs/spec/rbac.md §14): AccessAll, every
// section; AccessPeople, the users of kind local and oidc and the groups, for PCI DSS 7.2.4;
// AccessSystems, the service accounts, the workloads, the deploy tokens and the CI trust
// policies, for 7.2.5.1. The anonymous reads are in every scope.
const (
	AccessAll     = "all"
	AccessPeople  = "people"
	AccessSystems = "systems"
)

// AccessScopes are the scopes, in the order the messages name them.
var AccessScopes = []string{AccessAll, AccessPeople, AccessSystems}

// CheckAccessScope returns an error wrapping ErrInvalid, naming the field scope, unless scope is
// one of AccessScopes.
func CheckAccessScope(scope string) error {
	if !slices.Contains(AccessScopes, scope) {
		return invalidf("scope: %q is not all, people or systems", scope)
	}
	return nil
}

// ActionAccessReview is the audit action of a review of the access report (docs/spec/rbac.md §5,
// §14): subject sha256:<hex> of the report, by the admin who records it.
const ActionAccessReview = "access.review"

// The bounds of a review: MaxReviewNote, the bytes of its note; ReviewAhead, how far after the
// database's time its generatedAt may be, the clocks' agreement of docs/operations.md.
const (
	MaxReviewNote = 4096
	ReviewAhead   = 60 * time.Second
)

// AccessReview is what a person records of an access report it reviewed (docs/spec/rbac.md §14):
// the sha256 of the document's bytes as it got them, 64 lowercase hexadecimal digits; the
// document's generatedAt and scope; and a note, what was found and done, who acknowledged, the
// ticket.
type AccessReview struct {
	SHA256      string
	GeneratedAt time.Time
	Scope       string
	Note        string
}

// check returns an error wrapping ErrInvalid that names the field of r out of its bounds.
func (r AccessReview) check() error {
	switch {
	case strings.HasPrefix(r.SHA256, "sha256:"):
		return invalidf("sha256: the 64 hexadecimal digits alone, without sha256:")
	case len(r.SHA256) != 64 || strings.Trim(r.SHA256, "0123456789abcdef") != "":
		return invalidf("sha256: %d characters; want the sha256 of the document's bytes, 64 lowercase hexadecimal digits",
			len(r.SHA256))
	case r.GeneratedAt.IsZero():
		return invalidf("generatedAt: required, the generatedAt of the document reviewed")
	}
	if err := CheckAccessScope(r.Scope); err != nil {
		return err
	}
	switch {
	case r.Note == "":
		return invalidf("note: required: what was found and done, who acknowledged, the ticket; 1 byte at least")
	case len(r.Note) > MaxReviewNote:
		return invalidf("note: %d bytes, at most %d", len(r.Note), MaxReviewNote)
	case !utf8.ValidString(r.Note):
		return invalidf("note: not UTF-8")
	case strings.ContainsFunc(r.Note, func(c rune) bool { return c != '\n' && unicode.IsControl(c) }):
		return invalidf("note: a control character (a tab, a carriage return); a line feed is the one it may hold")
	}
	return nil
}

// RecordAccessReview checks r and writes its access.review row on st, in a transaction of its
// own, by the principal of ctx, at the database's time (docs/spec/rbac.md §5, §14): subject
// sha256:<hex>, no repo, details {sha256, generatedAt, scope, note} with the origin o and the
// sign-in of a web UI's token (recordAudit). It returns the row as written. A field out of its
// bounds, and a generatedAt more than ReviewAhead after the database's time, are errors wrapping
// ErrInvalid that name the field, and nothing is written. Lajstrom keeps no report and does not
// compare the sha256 with one: the report changes with every use of a token. Who may record one,
// the admin role, is the caller's to decide.
func RecordAccessReview(ctx context.Context, st index.Store, r AccessReview, o Origin) (index.AuditEvent, error) {
	if err := r.check(); err != nil {
		return index.AuditEvent{}, err
	}
	now, err := st.DatabaseTime(ctx)
	if err != nil {
		return index.AuditEvent{}, fmt.Errorf("auth: record a review: the database's time: %w", err)
	}
	if latest := now.Add(ReviewAhead); r.GeneratedAt.After(latest) {
		return index.AuditEvent{}, invalidf("generatedAt: %s is after the server's time, %s: the generatedAt of the document "+
			"reviewed, which is never ahead of the server", r.GeneratedAt.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	ev := index.AuditEvent{At: now, Action: ActionAccessReview, Subject: "sha256:" + r.SHA256}
	details := map[string]any{"sha256": r.SHA256, "generatedAt": r.GeneratedAt.UTC(), "scope": r.Scope, "note": r.Note}
	ev, err = recordAudit(ctx, st, nil, ev, o, details)
	if err != nil {
		return index.AuditEvent{}, fmt.Errorf("auth: record a review: %w", err)
	}
	return ev, nil
}
