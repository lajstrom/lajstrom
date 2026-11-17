package main

import (
	"context"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/auth"
)

// observeAudit makes st write each audit row that it commits as a log line on standard error,
// unless audit.log is false (docs/spec/rbac.md §5, a line per row; #1278): msg audit at info
// whatever log.level says, in the format of every line (log.format), the row as GET
// /api/v1/audit answers it (audit.Lines). serve and the commands against the database
// (openMigrated) call it before they use st.
func (a *app) observeAudit(st store) {
	if a.cfg == nil || !a.cfg.Audit.Log {
		return
	}
	kind := func(ctx context.Context, principal string) (string, error) {
		return auth.PrincipalKind(ctx, st, principal)
	}
	st.ObserveAudit(audit.Lines(slog.New(atInfo{a.log.Handler()}), kind))
}

// atInfo passes the records of info and above to its handler whatever the handler's level: the
// audit lines go through the process's handler, its writer and its format, and log.level does
// not drop them.
type atInfo struct{ slog.Handler }

// Enabled implements slog.Handler.
func (h atInfo) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

// WithAttrs implements slog.Handler.
func (h atInfo) WithAttrs(attrs []slog.Attr) slog.Handler { return atInfo{h.Handler.WithAttrs(attrs)} }

// WithGroup implements slog.Handler.
func (h atInfo) WithGroup(name string) slog.Handler { return atInfo{h.Handler.WithGroup(name)} }
