package api

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Revisions (docs/api.md, Revisions; #796): a repo, a user, a webhook and a CI trust policy of
// the API have a revision, the revision column of their row, which every update of the row moves.
// The ETag of a GET of one, and of the answer of its POST, PATCH or PUT, names it; If-Match makes
// its changes conditional, and If-None-Match its GET.

// The tables whose rows have a revision, the first argument of ETag and IfMatch.
const (
	TableRepos      = "repos"
	TableUsers      = "users"
	TableWebhooks   = "webhooks"
	TableCIPolicies = "ci_policies"
)

// ETag returns the entity tag of the row id of table at revision: a strong tag, opaque, the
// first 12 bytes of a sha256 of the three in base64url. It changes with the revision, and two
// rows never share one: a repo deleted and created again under its name has another id.
func ETag(table string, id, revision int64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d/%d", table, id, revision))
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`
}

// IfMatch returns the condition (index.WithCondition) of the If-Match value v on a row of table,
// which what names for a person ("repo raw/scratch"): the row's ETag must be one of the tags of
// v, by strong comparison, so that a weak tag never matches; * matches every row, and returns a
// nil condition. A v that is not * or a list of entity tags is an error wrapping
// format.ErrInvalid. The CLI's --if-match takes it too.
func IfMatch(table, what, v string) (func(id, revision int64) error, error) {
	tags, err := parseETags("If-Match", v)
	if err != nil || tags == nil {
		return nil, err
	}
	return func(id, revision int64) error {
		now := ETag(table, id, revision)
		if slices.Contains(tags, now) {
			return nil
		}
		return &PreconditionError{What: what, ETag: now}
	}, nil
}

// PreconditionError is the error of a change whose If-Match does not name the revision that the
// row has: 412 CONFLICT, with the row's ETag now in the answer's ETag. Nothing changed.
type PreconditionError struct {
	What string // the resource, for a person: "repo raw/scratch", "user alice", "webhook 3"
	ETag string // the row's ETag now
}

// Error says what changed, and its ETag now.
func (e *PreconditionError) Error() string {
	return fmt.Sprintf("%s changed since it was read: its ETag is now %s; read it again and send the change again", e.What, e.ETag)
}

// Unwrap returns format.ErrPrecondition, which format.StatusOf answers 412.
func (e *PreconditionError) Unwrap() error { return format.ErrPrecondition }

// conditional returns r with the condition of its If-Match on the row of table that what names
// in its context (IfMatch), or r as it is without the header. A value that does not parse is
// 400, never ignored.
func conditional(r *http.Request, table, what string) (*http.Request, error) {
	v := r.Header.Values("If-Match")
	if len(v) == 0 {
		return r, nil
	}
	cond, err := IfMatch(table, what, strings.Join(v, ", "))
	if err != nil || cond == nil {
		return r, err
	}
	return r.WithContext(index.WithCondition(r.Context(), cond)), nil
}

// tagged answers body with its ETag, etag: 200, or 304 Not Modified without a body when the
// request's If-None-Match has etag, by weak comparison, or is *. A value that does not parse is
// 400.
func tagged(w http.ResponseWriter, r *http.Request, etag string, body any) (int, any, error) {
	if v := r.Header.Values("If-None-Match"); len(v) > 0 {
		tags, err := parseETags("If-None-Match", strings.Join(v, ", "))
		if err != nil {
			return 0, nil, err
		}
		w.Header().Set("ETag", etag)
		if tags == nil || slices.ContainsFunc(tags, func(t string) bool { return strings.TrimPrefix(t, "W/") == etag }) {
			return http.StatusNotModified, nil, nil
		}
		return http.StatusOK, body, nil
	}
	w.Header().Set("ETag", etag)
	return http.StatusOK, body, nil
}

// parseETags returns the entity tags of the value v of the header h, each as it was sent, W/
// included; nil for *. Empty elements of the list are skipped (RFC 9110 §5.6.1). A value that
// is neither * nor a list of tags is an error wrapping format.ErrInvalid.
func parseETags(h, v string) ([]string, error) {
	if strings.TrimSpace(v) == "*" {
		return nil, nil
	}
	tags := []string{}
	for _, t := range strings.Split(v, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		opaque := strings.TrimPrefix(t, "W/")
		if len(opaque) < 2 || opaque[0] != '"' || opaque[len(opaque)-1] != '"' || strings.Contains(opaque[1:len(opaque)-1], `"`) {
			return nil, invalid(`%s: %q is not * or a list of entity tags, such as the "…" of the ETag a GET answered`, h, v)
		}
		tags = append(tags, t)
	}
	if len(tags) == 0 {
		return nil, invalid(`%s: %q is not * or a list of entity tags, such as the "…" of the ETag a GET answered`, h, v)
	}
	return tags, nil
}
