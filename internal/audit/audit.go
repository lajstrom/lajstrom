package audit

import (
	"context"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Writer records one audit event: a write, a control-plane change or a token issuance
// (docs/spec/rbac.md §5). The implementation (issue #46) stores it through index.Writer.
type Writer interface {
	// Write records ev. A zero ev.At means now.
	Write(ctx context.Context, ev index.AuditEvent) error
}
