package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// AuditList is the answer of GET /api/v1/audit: a page of the events, newest first, and in
// Next the cursor of the page after it, absent on the last page.
type AuditList struct {
	Events []AuditEntry `json:"events"`
	Next   string       `json:"next,omitempty"`
}

// AuditEntry is an audit event as the API answers it (docs/spec/rbac.md §5), and as its log line
// holds it (audit.Lines).
type AuditEntry = audit.Entry

// listAudit answers a page of the audit events that match ?repo, ?principal, ?action, ?state,
// ?since (inclusive) and ?until (exclusive), newest first: ?limit of them after the event of
// ?cursor.
func (h *handler) listAudit(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	limit, after, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	f, ok, err := h.auditFilter(r)
	if err != nil || !ok {
		return http.StatusOK, AuditList{Events: []AuditEntry{}}, err
	}
	var from *auditKey
	if after != "" {
		if from, err = parseAuditKey(after); err != nil {
			return 0, nil, invalid("cursor: %q is not the next of a page of this list", r.URL.Query().Get("cursor"))
		}
	}
	evs, more, err := auditPage(r.Context(), h.repos.Index, f, limit, from)
	if err != nil {
		return 0, nil, err
	}
	list := AuditList{Events: make([]AuditEntry, len(evs))}
	kinds := map[string]string{}
	for i, ev := range evs {
		kind, err := h.principalKind(r.Context(), ev.Principal, kinds)
		if err != nil {
			return 0, nil, err
		}
		list.Events[i] = audit.NewEntry(ev, kind)
	}
	if more {
		last := evs[len(evs)-1]
		list.Next = cursor(auditKey{At: last.At, ID: last.ID}.String())
	}
	return http.StatusOK, list, nil
}

// principalKind returns the principalKind of principal (auth.PrincipalKind). kinds keeps the
// answers by principal, so that a page reads each user once.
func (h *handler) principalKind(ctx context.Context, principal string, kinds map[string]string) (string, error) {
	if kind, ok := kinds[principal]; ok {
		return kind, nil
	}
	kind, err := auth.PrincipalKind(ctx, h.repos.Index, principal)
	if err != nil {
		return "", err
	}
	kinds[principal] = kind
	return kind, nil
}

// auditFilter returns the index.AuditFilter of the query. A principal user:<name> is the live
// user of that name, by its uid; ok is false when there is no such user, so no event.
func (h *handler) auditFilter(r *http.Request) (f index.AuditFilter, ok bool, err error) {
	q := r.URL.Query()
	f = index.AuditFilter{Repo: q.Get("repo"), Action: q.Get("action"), State: q.Get("state")}
	switch f.State {
	case "", index.AuditOK, index.AuditPending, index.AuditFailed, index.AuditUnknown:
	default:
		return f, false, invalid("state: %q is not ok, pending, failed or unknown", f.State)
	}
	for _, t := range []struct {
		key string
		to  *time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if v := q.Get(t.key); v != "" {
			if *t.to, err = time.Parse(time.RFC3339, v); err != nil {
				return f, false, invalid("%s: %q is not an RFC 3339 time, like 2026-11-01T00:00:00Z", t.key, v)
			}
		}
	}
	if v := q.Get("principal"); v != "" {
		p, err := auth.ParsePrincipal(v)
		if err != nil {
			return f, false, invalid("principal: %s", err)
		}
		if p.Kind == auth.KindUser && !index.IsUID(p.Name) {
			u, err := auth.LookupUser(r.Context(), h.repos.Index, p)
			switch {
			case errors.Is(err, index.ErrNotFound):
				return f, false, nil
			case err != nil:
				return f, false, err
			}
			p = auth.UserPrincipal(u)
		}
		f.Principal = p.String()
	}
	return f, true, nil
}

// auditKey is the place of an event in the order of ListAudit, at then id, both descending.
type auditKey struct {
	At time.Time
	ID int64
}

// String is the key as a cursor holds it: the time in RFC 3339 with nanoseconds, |, the id.
func (k auditKey) String() string {
	return k.At.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(k.ID, 10)
}

func parseAuditKey(s string) (*auditKey, error) {
	at, id, ok := strings.Cut(s, "|")
	if !ok {
		return nil, errors.New("no |")
	}
	var k auditKey
	var err error
	if k.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
		return nil, err
	}
	if k.ID, err = strconv.ParseInt(id, 10, 64); err != nil {
		return nil, err
	}
	return &k, nil
}

// auditPage returns up to limit events of f after the event of from (nil: from the newest),
// and whether more follow. ListAudit pages by offset, which events written meanwhile would
// shift; the key does not move: the events are those at or before from's time, less those at
// its time that come before it in the order (an id not below its id). The index keeps times to
// the microsecond, so at or before from's time is before it plus a microsecond.
func auditPage(ctx context.Context, r index.Reader, f index.AuditFilter, limit int, from *auditKey) ([]index.AuditEvent, bool, error) {
	if from != nil && (f.Until.IsZero() || from.At.Before(f.Until)) {
		f.Until = from.At.Add(time.Microsecond)
	}
	f.Limit = limit + 1
	var out []index.AuditEvent
	for {
		evs, err := r.ListAudit(ctx, f)
		if err != nil {
			return nil, false, err
		}
		for _, ev := range evs {
			if from != nil && ev.At.Equal(from.At) && ev.ID >= from.ID {
				continue
			}
			if out = append(out, ev); len(out) > limit {
				return out[:limit], true, nil
			}
		}
		if len(evs) < f.Limit {
			return out, false, nil
		}
		f.Offset += len(evs)
	}
}
