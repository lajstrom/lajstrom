package probe

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// capabilitiesOf converts c.
func capabilitiesOf(c provider.Capabilities) Capabilities {
	o := Capabilities{CreateOnPush: c.CreateOnPush, Referrers: c.Referrers, Catalog: c.Catalog, DeleteManifest: c.DeleteManifest,
		ImmutableTags: c.ImmutableTags, BlobRedirect: c.BlobRedirect, MaxBlobSize: c.MaxBlobSize, BuildRoutes: c.BuildRoutes,
		Consistency: string(c.Consistency)}
	if c.ConsistencyBound > 0 {
		o.ConsistencyBound = c.ConsistencyBound.String()
	}
	return o
}

// providerLog is the slog handler of the generic provider that the run asks: its request lines
// become exchanges of the current cell, its capability line names the vendor, and every other
// record goes to next.
type providerLog struct {
	next   slog.Handler
	vendor *string
}

// Enabled implements slog.Handler: every level, so that the provider logs its requests.
func (h *providerLog) Enabled(context.Context, slog.Level) bool { return true }

// Handle implements slog.Handler.
func (h *providerLog) Handle(ctx context.Context, r slog.Record) error {
	attrs := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	switch r.Message {
	case "registry request", "registry request failed":
		if t := tapeOf(ctx); t != nil {
			e := &Exchange{Method: attrs["method"], URL: attrs["path"], Via: "provider", Error: attrs["error"]}
			_, _ = fmt.Sscan(attrs["status"], &e.Status)
			t.add(e)
		}
		return nil
	case "registry capabilities":
		*h.vendor = attrs["vendor"]
	}
	if !h.next.Enabled(ctx, r.Level) {
		return nil
	}
	return h.next.Handle(ctx, r)
}

// WithAttrs implements slog.Handler.
func (h *providerLog) WithAttrs(as []slog.Attr) slog.Handler {
	return &providerLog{next: h.next.WithAttrs(as), vendor: h.vendor}
}

// WithGroup implements slog.Handler.
func (h *providerLog) WithGroup(name string) slog.Handler {
	return &providerLog{next: h.next.WithGroup(name), vendor: h.vendor}
}
