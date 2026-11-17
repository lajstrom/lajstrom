package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Entry is an audit row as GET /api/v1/audit answers it, and as its log line holds it
// (docs/spec/rbac.md §5): one shape for the API, the feed and the log store.
type Entry struct {
	ID            int64     `json:"id"`
	At            time.Time `json:"at"`
	Principal     string    `json:"principal"`
	PrincipalName string    `json:"principalName,omitempty"`
	// PrincipalKind is service when the principal is a service account's user:<uid>
	// (docs/spec/rbac.md §5); absent otherwise.
	PrincipalKind string          `json:"principalKind,omitempty"`
	Action        string          `json:"action"`
	Repo          string          `json:"repo,omitempty"`
	Subject       string          `json:"subject,omitempty"`
	Digest        string          `json:"digest,omitempty"`
	IP            string          `json:"ip,omitempty"`
	Details       json.RawMessage `json:"details,omitempty"`
	// State is ok, or for a registry write pending, failed (Error says why) or unknown
	// (docs/spec/index-schema.md, audit).
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// NewEntry is the entry of the row ev, whose principal is of kind (Entry.PrincipalKind).
func NewEntry(ev index.AuditEvent, kind string) Entry {
	return Entry{ID: ev.ID, At: ev.At.UTC(), Principal: ev.Principal, PrincipalName: ev.PrincipalName, PrincipalKind: kind,
		Action: ev.Action, Repo: ev.Repo, Subject: ev.Subject, Digest: string(ev.Digest), IP: ev.IP, Details: ev.Details,
		State: ev.State, Error: ev.Error}
}

// Attrs are the fields of the log line of e, in the order of docs/spec/rbac.md §5, each left out
// when empty as the API leaves it out: at in RFC 3339 with its fraction, as the API writes it, and
// details whole, an object in the json format of log.format and its JSON text in the text format.
func (e Entry) Attrs() []slog.Attr {
	attrs := []slog.Attr{slog.Int64("id", e.ID), slog.String("at", e.At.UTC().Format(time.RFC3339Nano)),
		slog.String("principal", e.Principal)}
	for _, f := range []struct{ key, v string }{{"principalName", e.PrincipalName}, {"principalKind", e.PrincipalKind},
		{"action", e.Action}, {"repo", e.Repo}, {"subject", e.Subject}, {"digest", e.Digest}, {"ip", e.IP},
		{"state", e.State}, {"error", e.Error}} {
		if f.v != "" {
			attrs = append(attrs, slog.String(f.key, f.v))
		}
	}
	if len(e.Details) > 0 && string(e.Details) != "null" {
		attrs = append(attrs, slog.Any("details", e.Details))
	}
	return attrs
}

// LineMessage is the message of the log line of an audit row.
const LineMessage = "audit"

// maxKinds bounds the principals whose kind Lines keeps.
const maxKinds = 4096

// kindTimeout bounds a read of the kind of a principal that Lines does not know yet.
const kindTimeout = 5 * time.Second

// Lines returns the observer of the audit rows that a store commits (the drivers' ObserveAudit)
// that writes each row as one log line on log, msg audit at info, with the fields of its Entry
// (docs/spec/rbac.md §5, a line per row; #1278). The line is built from the committed row and
// holds nothing else. kind tells the principalKind of a principal (auth.PrincipalKind); Lines
// keeps its answers, since a user's kind never changes, and a row whose kind it cannot read has
// its line without one. The level and the format are log's; a line that log cannot write is
// lost, and changes nothing.
func Lines(log *slog.Logger, kind func(ctx context.Context, principal string) (string, error)) func([]index.AuditEvent) {
	var (
		mu    sync.Mutex
		kinds = map[string]string{}
	)
	kindOf := func(principal string) string {
		mu.Lock()
		k, ok := kinds[principal]
		mu.Unlock()
		if ok {
			return k
		}
		ctx, cancel := context.WithTimeout(context.Background(), kindTimeout)
		defer cancel()
		k, err := kind(ctx, principal)
		if err != nil {
			return ""
		}
		mu.Lock()
		if len(kinds) < maxKinds {
			kinds[principal] = k
		}
		mu.Unlock()
		return k
	}
	return func(rows []index.AuditEvent) {
		for _, ev := range rows {
			log.LogAttrs(context.Background(), slog.LevelInfo, LineMessage, NewEntry(ev, kindOf(ev.Principal)).Attrs()...)
		}
	}
}
