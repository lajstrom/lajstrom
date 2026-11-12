package generic

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// How the registry's upload status reads, once measured (measureStatus).
const (
	statusUnmeasured int32 = iota
	// statusExact is the specification's Range: 0-<position of the last byte>.
	statusExact
	// statusAhead is a Range one byte ahead, 0-<bytes held>: Quay 3.18.2 (bytes=0-<n>) and
	// Forgejo 17.0.1, whose PATCH answers are right (#804).
	statusAhead
)

const (
	// statusProbe is what the scratch session of measureStatus holds.
	statusProbe = "lajstrom"
	// statusCleanup bounds the cancel of the scratch session, which runs even when the request
	// that measured has ended.
	statusCleanup = 10 * time.Second
)

// statusOffset returns the offset of an upload status whose Range is rng, which offsetOf read
// as got, of the session prev, whose offset Lajstrom took from the registry's answer to the
// last request it sent the session, usually a PATCH. A status that agrees is got, on any
// registry. The first one that does not makes the provider measure how the registry reads its
// status (measureStatus); from then on, on a registry whose status is one byte ahead, every
// status is read one byte back. A status that still disagrees is logged: the session holds
// bytes Lajstrom did not count, or misses some.
func (r *repository) statusOffset(ctx context.Context, prev provider.Upload, rng string, got int64) int64 {
	last, ok := rangeLast(rng)
	if !ok {
		return got
	}
	conv := r.p.statusConv.Load()
	if conv == statusUnmeasured && got != prev.Offset {
		conv = r.p.measureStatus(ctx, r)
	}
	off := got
	if conv == statusAhead {
		off = last
		r.p.log.DebugContext(ctx, "upload status read one byte back", "upload", prev.ID, "range", rng)
	}
	if off != prev.Offset {
		r.p.log.WarnContext(ctx, "an upload status disagrees with the offset of the session's last answer",
			"upload", prev.ID, "offset", prev.Offset, "range", rng, "statusAhead", conv == statusAhead, "taken", off)
	}
	return off
}

// measureStatus returns how the registry reads its upload status, measured once for the
// provider: a scratch session in r gets statusProbe in one PATCH, its status is asked, and it is
// cancelled. A Range that ends at len(statusProbe) is one byte ahead; any other is taken as the
// specification's. A measurement that fails is logged and not kept, and the status is then
// taken as it reads.
func (p *Provider) measureStatus(ctx context.Context, r *repository) int32 {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	if c := p.statusConv.Load(); c != statusUnmeasured {
		return c
	}
	c, err := r.measureStatus(ctx)
	if err != nil {
		p.log.WarnContext(ctx, "could not measure how the registry answers an upload status; taking it as it reads", "error", err.Error())
		return statusUnmeasured
	}
	p.statusConv.Store(c)
	if c == statusAhead {
		p.log.WarnContext(ctx, "the registry answers an upload status one byte ahead of the bytes the session holds; "+
			"the provider reads every status one byte back")
	}
	return c
}

// measureStatus runs measureStatus's scratch session in r.
func (r *repository) measureStatus(ctx context.Context) (int32, error) {
	u, err := r.StartBlobUpload(ctx)
	if err != nil {
		return statusUnmeasured, err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusCleanup)
		defer cancel()
		if err := r.CancelBlobUpload(cctx, u); err != nil {
			r.p.log.DebugContext(ctx, "could not cancel the session that measured the upload status", "error", err.Error())
		}
	}()
	if u, err = r.AppendBlobChunk(ctx, u, strings.NewReader(statusProbe), int64(len(statusProbe))); err != nil {
		return statusUnmeasured, err
	}
	op := r.op("measure upload status", u.ID)
	loc, err := r.p.location(u.Location)
	if err != nil {
		return statusUnmeasured, err
	}
	resp, err := r.send(ctx, op, http.MethodGet, loc.String(), nil, 0, nil, http.StatusNoContent, http.StatusOK)
	if err != nil {
		return statusUnmeasured, err
	}
	_ = resp.Body.Close()
	if last, ok := rangeLast(resp.Header.Get("Range")); ok && last == int64(len(statusProbe)) {
		return statusAhead, nil
	}
	return statusExact, nil
}
