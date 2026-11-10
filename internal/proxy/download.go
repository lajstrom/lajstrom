package proxy

import (
	"context"
	"crypto/sha1" //nolint:gosec // an upstream's sha1 is checked when it gives nothing stronger, not trusted as a security use
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// checksumOrder is the order of strength of an upstream's checksums: the first one an entry
// gives is the one verified (§5, step 8).
var checksumOrder = []string{"sha512", "sha256", "sha1"}

// errTooLarge is a download that went past its bound.
var errTooLarge = errors.New("proxy: the file is larger than its bound")

// downloaded are the spooled files of one artifact and its origin.
type downloaded struct {
	spools []*format.Spooled
	files  []format.UploadedFile
	origin *format.Origin
}

func (d *downloaded) close() {
	for _, s := range d.spools {
		_ = s.Close()
	}
}

// download requests each file of the entry from the upstream into a spool, as an upload is
// spooled, bounded by the repo's maxFileSize and the registry's maxBlobSize, and checks the
// strongest checksum the entry gives against the bytes (§5, steps 7 and 8). The spools are
// never served from; the caller closes them, which removes them. A refused file is a
// proxy.reject audit row; an upstream 404 or 410 makes the name's row stale (§4.3).
func (c *Core) download(ctx context.Context, p *proxied, a artifact, entry *format.UpstreamEntry, row *index.ProxyMeta) (_ *downloaded, err error) {
	limit, bound := c.limit(ctx, p)
	d := &downloaded{}
	defer func() {
		if err != nil {
			d.close()
		}
	}()
	for i, f := range entry.Files {
		target, err := p.up.Resolve(f.URL)
		if err != nil {
			return nil, err
		}
		asked := originURL(target)
		alg, want := strongest(f.Checksums)
		if alg == "" {
			c.reject(ctx, p, a, asked, "checksum", map[string]any{"error": "the upstream gives no checksum"})
			return nil, fmt.Errorf("%w: proxy: %s: upstream %s gives no checksum of %s", format.ErrBadGateway, p.id(), p.settings.Upstream, f.Name)
		}
		sp, got, err := c.spool(ctx, p, a, f.Name, target, alg, limit, bound, row)
		if err != nil {
			return nil, err
		}
		d.spools = append(d.spools, sp)
		d.files = append(d.files, sp.File())
		if got != want {
			c.reject(ctx, p, a, asked, "checksum", map[string]any{"algorithm": alg, "expected": want, "actual": got})
			c.o.Log.WarnContext(ctx, "proxy: a file of the upstream does not match its checksum; refused", "repo", p.id(),
				"upstream", p.settings.Upstream, "artifact", a.subject(), "file", f.Name, "algorithm", alg, "expected", want, "actual", got)
			return nil, fmt.Errorf("%w: proxy: %s: %s of upstream %s does not match its %s", format.ErrBadGateway, p.id(), f.Name,
				p.settings.Upstream, alg)
		}
		if i == 0 {
			d.origin = &format.Origin{Upstream: p.settings.Upstream, URL: asked, Checksum: alg + ":" + want}
		}
	}
	return d, nil
}

// spool downloads one file into a spool and returns it with the checksum alg of its bytes.
func (c *Core) spool(ctx context.Context, p *proxied, a artifact, name string, target *url.URL, alg string, limit int64, bound string,
	row *index.ProxyMeta) (*format.Spooled, string, error) {
	resp, err := p.up.Get(ctx, "file", target, "", "", "")
	switch {
	case errors.Is(err, errMissing):
		c.stale(ctx, p, row)
		countFetch(p, fetchMissing)
		return nil, "", fmt.Errorf("%w: proxy: %s: upstream %s does not have %s", index.ErrNotFound, p.id(), p.settings.Upstream, name)
	case err != nil:
		countFetch(p, fetchError)
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if limit >= 0 && resp.ContentLength > limit {
		return nil, "", c.tooLarge(ctx, p, a, target, limit, bound)
	}
	sums := map[string]hash.Hash{"sha512": sha512.New(), "sha256": sha256.New(), "sha1": sha1.New()} //nolint:gosec // see the import
	writers := make([]io.Writer, 0, len(sums))
	for _, h := range sums {
		writers = append(writers, h)
	}
	body := io.TeeReader(&bounded{r: resp.Body, left: limit}, io.MultiWriter(writers...))
	sp, err := format.Spool(body, name, "", -1)
	switch {
	case errors.Is(err, errTooLarge):
		return nil, "", c.tooLarge(ctx, p, a, target, limit, bound)
	case err != nil:
		countFetch(p, fetchError)
		return nil, "", p.up.transportError(ctx, err)
	}
	return sp, hex.EncodeToString(sums[alg].Sum(nil)), nil
}

// tooLarge refuses a file above its bound with 403 naming the bound, and a proxy.reject row.
func (c *Core) tooLarge(ctx context.Context, p *proxied, a artifact, target *url.URL, limit int64, bound string) error {
	c.reject(ctx, p, a, originURL(target), "size", map[string]any{"limit": limit, "bound": bound})
	return fmt.Errorf("%w: proxy: %s: the file of %s is larger than %s, %d bytes", format.ErrForbidden, p.id(), a.subject(), bound, limit)
}

// limit is the bound of a download, and what it is: the repo's maxFileSize, or the registry's
// maxBlobSize when that is smaller; -1 when there is none.
func (c *Core) limit(ctx context.Context, p *proxied) (int64, string) {
	limit, bound := int64(-1), ""
	if n, err := format.MaxFileSize(p.repo); err == nil && n >= 0 {
		limit, bound = n, "the maxFileSize of "+p.id()
	}
	if c.o.Providers == nil {
		return limit, bound
	}
	if prov, ok := c.o.Providers.Provider(p.repo.Registry); ok {
		if caps, err := prov.Capabilities(ctx); err == nil && caps.MaxBlobSize > 0 && (limit < 0 || caps.MaxBlobSize < limit) {
			limit, bound = caps.MaxBlobSize, "the maxBlobSize of registry "+p.repo.Registry
		}
	}
	return limit, bound
}

// bounded reads r and fails with errTooLarge past left bytes; left -1 means no bound.
type bounded struct {
	r    io.Reader
	left int64
}

func (b *bounded) Read(p []byte) (int, error) {
	if b.left < 0 {
		return b.r.Read(p)
	}
	if b.left == 0 {
		// One byte more tells a body of exactly the bound from a larger one.
		var one [1]byte
		if n, _ := b.r.Read(one[:]); n > 0 {
			return 0, errTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

// strongest returns the strongest checksum of sums, as checksumOrder ranks them.
func strongest(sums map[string]string) (alg, sum string) {
	for _, a := range checksumOrder {
		if s := sums[a]; s != "" {
			return a, s
		}
	}
	return "", ""
}

// originURL is lajstrom.origin.url of a file asked at u: without userinfo, query or fragment
// (manifest-profile.md §3.1).
func originURL(u *url.URL) string {
	v := *u
	v.User, v.RawQuery, v.ForceQuery, v.Fragment, v.RawFragment = nil, "", false, "", ""
	return v.String()
}

// stale makes the row of a name stale after the upstream answered 404 or 410 for one of its
// files (§4.3): the next read asks the upstream again.
func (c *Core) stale(ctx context.Context, p *proxied, row *index.ProxyMeta) {
	if row == nil {
		return
	}
	if err := c.o.Index.TouchProxyMeta(ctx, row.ID, time.Unix(0, 0)); err != nil && !errors.Is(err, index.ErrNotFound) {
		c.o.Log.WarnContext(ctx, "proxy: a cached document not made stale", "repo", p.id(), "name", row.Name, "error", err.Error())
	}
}

// reject writes the audit row proxy.reject of a fetch refused for its bytes (rbac.md §7): the
// principal of the request, the client's address, and the reason with what details says.
func (c *Core) reject(ctx context.Context, p *proxied, a artifact, asked, reason string, details map[string]any) {
	countFetch(p, fetchRejected)
	details["upstream"], details["url"], details["reason"] = p.settings.Upstream, asked, reason
	if id := server.RequestID(ctx); id != "" {
		details["requestId"] = id
	}
	ev := index.AuditEvent{At: c.o.Now(), Principal: auth.PrincipalFrom(ctx).String(), Action: "proxy.reject", Repo: p.id(),
		Subject: a.subject(), IP: server.ClientAddr(ctx)}
	ev.Details = marshal(details)
	var err error
	if c.o.Audit != nil {
		err = c.o.Audit.Write(ctx, ev)
	} else {
		err = c.o.Index.AppendAudit(context.WithoutCancel(ctx), &ev)
	}
	if err != nil {
		c.o.Log.ErrorContext(ctx, "proxy: audit row of a refused fetch not written", "repo", p.id(), "artifact", a.subject(),
			"error", err.Error())
	}
}

// marshal is the JSON of audit details, which hold strings and integers only.
func marshal(details map[string]any) json.RawMessage {
	b, _ := json.Marshal(details) // strings and integers always marshal
	return b
}
