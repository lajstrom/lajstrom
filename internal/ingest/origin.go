package ingest

import (
	"context"
	"encoding/json"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// withOrigin completes ev, an audit event that ingest writes, with what the request of ctx
// tells about it, as the rows of the API have it (docs/spec/rbac.md §5). An empty ev.IP becomes
// the client address of the request (server.ClientAddr: the client that X-Forwarded-For names
// behind a trusted proxy, ADR 0011). The request id (server.RequestID) joins details as
// "requestId", the group a request came through (format.ViaFrom, rbac.md §7) as "via",
// <format>/<group>, and details become ev.Details, which stays nil when nothing is in them.
// Outside a request through the server's middleware, as in reindex or the CLI, ev keeps what
// it has.
func withOrigin(ctx context.Context, ev *index.AuditEvent, details map[string]any) {
	if ev.IP == "" {
		ev.IP = server.ClientAddr(ctx)
	}
	add := func(k, v string) {
		if details == nil {
			details = make(map[string]any, 2)
		}
		details[k] = v
	}
	if id := server.RequestID(ctx); id != "" {
		add("requestId", id)
	}
	if group, ok := format.ViaFrom(ctx); ok {
		add("via", group.Format+"/"+group.Name)
	}
	if len(details) > 0 {
		ev.Details, _ = json.Marshal(details) // strings and ints always marshal
	}
}
