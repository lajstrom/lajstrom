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
	"strings"
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

// download asks the upstream for the file of the entry (npm: the tarball) and returns its body
// as UploadStream reads it into the registry (§5, steps 7 and 8; ADR 0014): bounded by the
// repo's maxFileSize and the registry's maxBlobSize, and checked against the strongest checksum
// the entry gives when it ends. Nothing of it is written to disk; the caller closes the body. A
// file refused before a byte is read is a proxy.reject audit row, and an upstream 404 or 410
// makes the name's row stale (§4.3).
func (c *Core) download(ctx context.Context, p *proxied, a artifact, entry *format.UpstreamEntry, row *index.ProxyMeta) (*checked, error) {
	if err := c.checkName(ctx, p, a); err != nil {
		return nil, err
	}
	if len(entry.Files) != 1 {
		return nil, fmt.Errorf("proxy: %s: the entry of %s lists %d files; a fetch streams one into the registry", p.id(), a.subject(),
			len(entry.Files))
	}
	f := entry.Files[0]
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
	limit, bound := c.limit(ctx, p)
	resp, err := p.up.Get(ctx, "file", target, "", "", "")
	switch {
	case errors.Is(err, errMissing):
		c.stale(ctx, p, row)
		countFetch(p, fetchMissing)
		return nil, fmt.Errorf("%w: proxy: %s: upstream %s does not have %s", index.ErrNotFound, p.id(), p.settings.Upstream, f.Name)
	case err != nil:
		countFetch(p, fetchError)
		return nil, err
	}
	if limit >= 0 && resp.ContentLength > limit {
		_ = resp.Body.Close()
		return nil, c.tooLarge(ctx, p, a, asked, limit, bound)
	}
	return &checked{body: resp.Body, r: &bounded{r: resp.Body, left: limit}, h: newHash(alg), name: f.Name, limit: limit, bound: bound,
		origin: &format.Origin{Upstream: p.settings.Upstream, URL: asked, Checksum: alg + ":" + want, Published: entry.Published}, alg: alg,
		want: want}, nil
}

// checked is the body of a file on its way from the upstream into the registry. It hashes the
// bytes with the algorithm of the upstream's strongest checksum and, at their end, fails in
// place of io.EOF when they do not match it (a *mismatch), so that UploadStream cancels the
// upload session and commits nothing (§5, step 8). A body past its bound fails with errTooLarge;
// one the upstream cuts short, with an *upstreamError.
type checked struct {
	body      io.Closer
	r         io.Reader // the bounded body
	h         hash.Hash
	name      string // the file's name, UploadedFile.Name
	limit     int64  // the bound, -1 for none, and what it is
	bound     string
	origin    *format.Origin
	alg, want string
	ended     bool   // the body ended whole
	got       string // the checksum alg of the bytes, once ended
}

// mismatch is a file whose bytes do not match the upstream's checksum.
type mismatch struct{ alg, want, got string }

func (m *mismatch) Error() string {
	return fmt.Sprintf("proxy: the bytes have %s %s, the upstream says %s", m.alg, m.got, m.want)
}

// upstreamError is a failure to read the body of the upstream's answer: a connection cut, a
// body shorter than its Content-Length, a stall.
type upstreamError struct{ err error }

func (e *upstreamError) Error() string { return "proxy: the upstream's body: " + e.err.Error() }
func (e *upstreamError) Unwrap() error { return e.err }

func (c *checked) Read(p []byte) (int, error) {
	if c.ended {
		return 0, c.end()
	}
	n, err := c.r.Read(p)
	c.h.Write(p[:n])
	switch {
	case errors.Is(err, io.EOF):
		c.ended, c.got = true, hex.EncodeToString(c.h.Sum(nil))
		return n, c.end()
	case err != nil && !errors.Is(err, errTooLarge):
		return n, &upstreamError{err: err}
	}
	return n, err
}

// end is what a read of the ended body returns: io.EOF, or a *mismatch.
func (c *checked) end() error {
	if c.got != c.want {
		return &mismatch{alg: c.alg, want: c.want, got: c.got}
	}
	return io.EOF
}

// finish reads the rest of a body that the format refused, which may have stopped before its
// end, and returns the *mismatch it ends with, nil otherwise: bytes that are not the upstream's
// are refused for their checksum, as when the whole file was checked before the ingest.
func (c *checked) finish() *mismatch {
	var m *mismatch
	if _, err := io.Copy(io.Discard, c); errors.As(err, &m) {
		return m
	}
	return nil
}

func (c *checked) close() { _ = c.body.Close() }

// newHash returns the hash of an upstream's checksum algorithm of checksumOrder.
func newHash(alg string) hash.Hash {
	switch alg {
	case "sha512":
		return sha512.New()
	case "sha256":
		return sha256.New()
	}
	return sha1.New() //nolint:gosec // see the import
}

// refused turns the error of the ingest of a fetched file into the fetch's (§5, §6): bytes that
// do not match the upstream's checksum and an artifact the format refuses are 502 and a
// proxy.reject row, a file past its bound 403 and a proxy.reject row, a body the upstream cut
// short the upstream's error. UploadStream committed nothing for any of them.
func (c *Core) refused(ctx context.Context, p *proxied, a artifact, body *checked, err error) error {
	if errors.Is(err, format.ErrInvalid) {
		if m := body.finish(); m != nil {
			err = m
		}
	}
	var m *mismatch
	var up *upstreamError
	switch {
	case errors.As(err, &m):
		c.reject(ctx, p, a, body.origin.URL, "checksum", map[string]any{"algorithm": m.alg, "expected": m.want, "actual": m.got})
		c.o.Log.WarnContext(ctx, "proxy: a file of the upstream does not match its checksum; refused", "repo", p.id(),
			"upstream", p.settings.Upstream, "artifact", a.subject(), "file", body.name, "algorithm", m.alg, "expected", m.want, "actual", m.got)
		return fmt.Errorf("%w: proxy: %s: %s of upstream %s does not match its %s", format.ErrBadGateway, p.id(), body.name,
			p.settings.Upstream, m.alg)
	case errors.Is(err, errTooLarge):
		return c.tooLarge(ctx, p, a, body.origin.URL, body.limit, body.bound)
	case errors.As(err, &up):
		countFetch(p, fetchError)
		return p.up.transportError(ctx, up.err)
	case errors.Is(err, format.ErrInvalid):
		c.reject(ctx, p, a, body.origin.URL, "invalid", map[string]any{"error": err.Error()})
		return fmt.Errorf("%w: proxy: %s: upstream %s served %s, which the format refuses: %s", format.ErrBadGateway,
			p.id(), p.settings.Upstream, a.subject(), err.Error())
	}
	return err
}

// tooLarge refuses a file above its bound with 403 naming the bound, and a proxy.reject row.
func (c *Core) tooLarge(ctx context.Context, p *proxied, a artifact, asked string, limit int64, bound string) error {
	c.reject(ctx, p, a, asked, "size", map[string]any{"limit": limit, "bound": bound})
	return fmt.Errorf("%w: proxy: %s: the file of %s is larger than %s, %d bytes", format.ErrForbidden, p.id(), a.subject(), bound, limit)
}

// checkName refuses the artifact with 403 naming the limit, and a proxy.reject row, when the
// registry of the repo does not take the name of its OCI repository (provider.Capabilities.
// CheckName: the depth of a name on Docker Hub or behind GitLab; CheckNew for a package the index
// does not have: a name that ends in a build route on Quay, #662), before anything is
// downloaded: ingest would not publish it, and the upstream is not at fault (#599).
func (c *Core) checkName(ctx context.Context, p *proxied, a artifact) error {
	if c.o.Providers == nil {
		return nil
	}
	prov, ok := c.o.Providers.Provider(p.repo.Registry)
	if !ok {
		return nil
	}
	caps, err := prov.Capabilities(ctx)
	if err != nil {
		return nil //nolint:nilerr // the publish answers for the registry
	}
	base := strings.Trim(p.repo.Prefix, "/")
	if base == "" {
		base = p.repo.Format + "/" + p.repo.Name
	}
	name := base + "/" + a.pkgPath
	err = caps.CheckName(name)
	if refused := caps.CheckNew(name); err == nil && refused != nil {
		// A package the index has keeps taking versions: the registry holds its repository.
		switch _, lookup := index.PackageAt(ctx, c.o.Index, p.repo.ID, a.pkgPath); {
		case errors.Is(lookup, index.ErrNotFound):
			err = refused
		case lookup != nil:
			return fmt.Errorf("proxy: %s: %s: whether the index has the package: %w", p.id(), a.subject(), lookup)
		}
	}
	if err != nil {
		c.reject(ctx, p, a, "", "name", map[string]any{"error": err.Error()})
		return fmt.Errorf("%w: proxy: %s: %s: %s", format.ErrForbidden, p.id(), a.subject(), err.Error())
	}
	return nil
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
// principal of the request, the client's address, the group it came through, and the reason
// with what details says.
func (c *Core) reject(ctx context.Context, p *proxied, a artifact, asked, reason string, details map[string]any) {
	countFetch(p, fetchRejected)
	details["upstream"], details["url"], details["reason"] = p.settings.Upstream, asked, reason
	if id := server.RequestID(ctx); id != "" {
		details["requestId"] = id
	}
	if group, ok := format.ViaFrom(ctx); ok {
		details["via"] = group.Format + "/" + group.Name
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
