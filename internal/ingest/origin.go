package ingest

import (
	"context"
	"encoding/json"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// withOrigin completes ev, an audit event that ingest writes, with what the request of ctx
// tells about it, as the rows of the API have it (docs/spec/rbac.md §5). An empty ev.IP becomes
// the client address of the request (server.ClientAddr: the client that X-Forwarded-For names
// behind a trusted proxy, ADR 0011). The request id (server.RequestID) joins details as
// "requestId", the group a request came through (format.ViaFrom, rbac.md §7) as "via",
// <format>/<group>, the id of a CI token (auth.CITokenFrom, rbac.md §11) as "ciToken", the reason
// of the change that the caller stored (auth.OriginFrom; a deny's, rbac.md §13) as "changeReason",
// and details become ev.Details, which stays nil when nothing is in them.
// Outside a request through the server's middleware, as in reindex or the CLI, ev keeps what
// it has, and the reason of the command line when it has one.
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
	if id := auth.CITokenFrom(ctx); id != "" {
		add("ciToken", id)
	}
	if reason := auth.OriginFrom(ctx).ChangeReason; reason != "" {
		add("changeReason", reason)
	}
	if len(details) > 0 {
		ev.Details, _ = json.Marshal(details) // strings and ints always marshal
	}
}
