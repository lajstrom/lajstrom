package generic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

const (
	// probeRepository is the repository, below the instance prefix, whose referrers the probe
	// asks for. It need not exist: a registry with the referrers API answers an empty index.
	probeRepository = "lajstrom-probe"
	// zeroDigest is the subject of the referrers probe; no manifest has it.
	zeroDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	// maxCatalogProbeBody bounds how much of the catalog probe's answer, one entry, is read.
	maxCatalogProbeBody = 64 << 10
	// minNameDepth is the depth of the shortest name Lajstrom writes below the prefix,
	// <format>/<repo>/<package> (docs/spec/manifest-profile.md §4.1).
	minNameDepth = 3
	// maxNameLen is the most bytes of a repository name, the instance prefix included:
	// distribution's RepositoryNameTotalLengthMax, which GitLab's registry keeps too
	// (docs/registries/quirks.md, Case and length). Harbor takes fewer: vendorNameLengths.
	maxNameLen = 255
)

// Capabilities probes the registry on the first call and caches the answer; a probe that
// fails is not cached, so the next call probes again. Concurrent calls, and the blob reads
// that probe before the provider's first request (readQuirks), wait for one probe. The vendor
// table (vendor.go) sets what the probe cannot measure, and capabilities: from the
// configuration replace both. The probe is:
//
//   - reachability: GET /v2/; a failure is the error of Capabilities. Its headers, the
//     challenge of a 401 the auth client answered on the way, and the host of the URL name
//     the vendor (detectVendor);
//   - catalog: GET /v2/_catalog?n=1, with the credentials (catalogClient); 200 with a
//     catalog (a JSON object with repositories) is true; a 200 without one, 401, 403, 404 and
//     405 are false;
//   - referrers: GET /v2/<prefix>lajstrom-probe/referrers/<zero digest>; 200 with an OCI
//     image index is true, any other 200 and a 404 without NAME_UNKNOWN are false, and so is
//     an answer that does not tell (404 NAME_UNKNOWN, 401, 403), in which case oras-go decides
//     per repository on first use;
//   - CreateOnPush, DeleteManifest and BlobRedirect are true, ImmutableTags and TagsFoldCase
//     are false, MaxBlobSize is 0 (unknown) and Consistency unknown without a request; DeleteManifest
//     turns false when the registry answers a manifest delete with 405 UNSUPPORTED or a 405
//     without DENIED.
//
// MaxNameLength is maxNameLen, or the vendor table's bound (vendorNameLengths: Harbor's 245), less
// the length of the instance prefix, measured by no request.
//
// A capability that the vendor table or the configuration sets is not probed. What the vendor
// table decides from the probe's answer (afterProbe) comes before the configuration's overrides,
// with the vendor's quirks: the blob reads of Nexus's oci format, JFrog's blob HEAD and its
// upload status without bytes, and the token scope of Quay's deletes. The
// first successful probe logs the result and the vendor at info, hints at
// registries[].uploads.spoolUnknownLength for Gitea and JFrog when it is off, and warns about a
// GitLab registry with its metadata database, which refuses artifact types it does not know,
// and about an eventually consistent registry whose lag has no bound in the configuration.
func (p *Provider) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	c, err := p.probeOnce(ctx)
	if err != nil {
		return provider.Capabilities{}, err
	}
	return p.effective(c), nil
}

// nameLength is the MaxNameLength below prefix, as it is prepended to names ("" or ending in
// "/"), on a registry of vendor v: maxNameLen, or the vendor table's bound (vendorNameLengths),
// less its length, and at least 1, so that a prefix that leaves no room refuses every name
// rather than lifting the limit.
func nameLength(prefix string, v vendor) int {
	bound := maxNameLen
	if n, ok := vendorNameLengths[v]; ok {
		bound = n
	}
	return max(bound-len(prefix), 1)
}

// effective applies what Lajstrom learnt since the probe, then the overrides, to probed,
// which holds the vendor table's capabilities already.
func (p *Provider) effective(probed provider.Capabilities) provider.Capabilities {
	c := probed
	if p.deleteRefused.Load() {
		c.DeleteManifest = false
	}
	apply(&c, p.override)
	return c
}

// apply sets in c the capabilities that o sets: the vendor table's, or the configuration's.
func apply(c *provider.Capabilities, o config.CapabilityOverrides) {
	set := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	set(&c.CreateOnPush, o.CreateOnPush)
	set(&c.Referrers, o.Referrers)
	set(&c.Catalog, o.Catalog)
	set(&c.DeleteManifest, o.DeleteManifest)
	set(&c.ImmutableTags, o.ImmutableTags)
	set(&c.BlobRedirect, o.BlobRedirect)
	set(&c.TagsFoldCase, o.TagsFoldCase)
	set(&c.BuildRoutes, o.BuildRoutes)
	if o.MaxBlobSize > 0 {
		c.MaxBlobSize = o.MaxBlobSize
	}
	if o.Consistency != "" {
		c.Consistency = provider.Consistency(o.Consistency)
	}
	if o.ConsistencyBound > 0 {
		c.ConsistencyBound = o.ConsistencyBound
	}
	if o.MaxNameDepth > 0 {
		c.MaxNameDepth = o.MaxNameDepth
	}
}

// probeBackoff is how long after a probe of its own that failed, though the registry answered
// its ping, the read path goes without probing (readQuirks); a variable so that tests can
// change it.
var probeBackoff = 10 * time.Second

// errProbeBackoff is probeShared's answer to the read path within probeBackoff of a probe of
// its own that failed: no new probe.
var errProbeBackoff = errors.New("generic: a probe before a read failed a moment ago")

// probeFlight is a probe under way; the callers that come while it runs wait for its answer.
type probeFlight struct {
	done chan struct{} // closed when the probe has ended
	err  error         // the probe's error; nil when it succeeded
	// unanswered: the registry did not answer the ping, provider.ErrUnavailable.
	unanswered bool
	// abandoned: the probe ended because the context of the caller that ran it did, not
	// with the registry's answer; a caller still waiting probes again.
	abandoned bool
}

// probeOnce returns the cached probe, or probes and caches a successful answer. Concurrent
// first calls wait for one probe and share its answer, a failure too; a call after a failure
// probes again.
func (p *Provider) probeOnce(ctx context.Context) (provider.Capabilities, error) {
	c, _, err := p.probeShared(ctx, 0, false)
	return c, err
}

// probeShared is probeOnce for a caller that waits at most wait for the probe, without a bound
// for 0, and that, with backoff, starts no probe within probeBackoff of one of its kind that
// failed (errProbeBackoff), but waits for one under way. A caller stops waiting when its
// context ends; when it ran the probe, a caller still waiting runs it again. unanswered says
// that the error is the ping's, which the registry did not answer.
func (p *Provider) probeShared(ctx context.Context, wait time.Duration, backoff bool) (c provider.Capabilities, unanswered bool, err error) {
	var deadline time.Time
	var expired <-chan time.Time
	if wait > 0 {
		deadline = time.Now().Add(wait)
		t := time.NewTimer(wait)
		defer t.Stop()
		expired = t.C
	}
	for {
		if c := p.probed.Load(); c != nil {
			return *c, false, nil
		}
		p.mu.Lock()
		f := p.flight
		if f == nil {
			if c := p.probed.Load(); c != nil { // a probe ended since the load above
				p.mu.Unlock()
				return *c, false, nil
			}
			if backoff && time.Since(p.failedAt) < probeBackoff {
				p.mu.Unlock()
				return provider.Capabilities{}, false, errProbeBackoff
			}
			f = &probeFlight{done: make(chan struct{})}
			p.flight = f
			p.mu.Unlock()
			c, err := p.lead(ctx, f, deadline, backoff)
			return c, f.unanswered, err
		}
		p.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return provider.Capabilities{}, false, p.translate("wait for the probe of "+p.name, ctx.Err())
		case <-expired:
			return provider.Capabilities{}, false, fmt.Errorf("generic: wait for the probe of %s: not done within %s", p.name, wait)
		}
		if f.err != nil && !f.abandoned {
			return provider.Capabilities{}, f.unanswered, f.err
		}
	}
}

// lead runs the probe of f under ctx, cut at deadline unless it is zero, and ends f with its
// answer. For a caller with backoff, a failure sets failedAt unless it is ctx's own or the
// unanswered ping's: running out of the wait is the registry's doing.
func (p *Provider) lead(ctx context.Context, f *probeFlight, deadline time.Time, backoff bool) (c provider.Capabilities, err error) {
	answered := false
	defer func() {
		p.mu.Lock()
		f.err, f.abandoned = err, err != nil && ctx.Err() != nil
		f.unanswered = err != nil && !answered && !f.abandoned && errors.Is(err, provider.ErrUnavailable)
		if backoff && err != nil && !f.abandoned && !f.unanswered {
			p.failedAt = time.Now()
		}
		p.flight = nil
		p.mu.Unlock()
		close(f.done)
	}()
	pctx := ctx
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		pctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	return p.probeNow(pctx, &answered)
}

// probeNow probes, and caches a successful answer with the vendor's quirks; probeShared runs
// one at a time. It sets answered once the registry answered the ping.
func (p *Provider) probeNow(ctx context.Context, answered *bool) (provider.Capabilities, error) {
	id, err := p.ping(ctx)
	if err != nil {
		return provider.Capabilities{}, p.translate("ping "+p.name, err)
	}
	*answered = true
	v := p.vendorOf(ctx, id)
	c, err := p.probe(ctx, vendorOverrides(v, id))
	if err != nil {
		return provider.Capabilities{}, err
	}
	c.MaxNameLength = nameLength(p.prefix, v)
	q := afterProbe(v, &c, p.effective(c).Referrers)
	p.quirks.Store(&q)
	p.probed.Store(&c)
	e := p.effective(c)
	p.log.InfoContext(ctx, "registry capabilities",
		"vendor", string(v),
		"createOnPush", e.CreateOnPush,
		"referrers", e.Referrers,
		"catalog", e.Catalog,
		"deleteManifest", e.DeleteManifest,
		"immutableTags", e.ImmutableTags,
		"blobRedirect", e.BlobRedirect,
		"maxBlobSize", e.MaxBlobSize,
		"consistency", consistencyText(e),
		"maxNameDepth", e.MaxNameDepth,
		"tagsFoldCase", e.TagsFoldCase,
		"buildRoutes", e.BuildRoutes,
	)
	if e.MaxNameDepth > 0 && e.MaxNameDepth < minNameDepth {
		p.log.ErrorContext(ctx, "the registry takes no name of Lajstrom's: every one has 3 components or more below the prefix, "+
			"<format>/<repo>/<package>, and a publish to the registry is refused", "vendor", string(v), "maxNameDepth", e.MaxNameDepth)
	}
	if e.Consistency == provider.ConsistencyEventual && e.ConsistencyBound == 0 {
		p.log.WarnContext(ctx, "the registry is eventually consistent and no bound of its lag is known: every publish waits "+
			"until the registry serves it, and answers 503 past the wait; `lajstrom registry probe` measures the lag, "+
			"registries[].capabilities.consistencyBound records it", "vendor", string(v))
	}
	if q != (quirks{}) {
		p.log.InfoContext(ctx, "registry needs workarounds", "vendor", string(v),
			"blobsReadWhole", q.wholeBlobs, "blobExistsByGET", q.blobExistsByGET, "pushForDelete", q.pushForDelete,
			"emptyStatus404", q.emptyStatus404)
	}
	if hintSpool(v) && !p.spoolUnknown {
		p.spoolHint.Store(true)
		p.log.InfoContext(ctx, "hint: the registry refuses a /v2 chunk of unknown length after the first write, "+
			"and such an upload fails with its 4xx; registries[].uploads.spoolUnknownLength spools that chunk to $TMPDIR",
			"vendor", string(v))
	}
	if v == vendorGitLab && id.gitLabDatabase() {
		p.log.WarnContext(ctx, "GitLab registry with its metadata database: it refuses a manifest whose artifactType or config media type it does not know, every one Lajstrom writes, unless REGISTRY_FF_DYNAMIC_MEDIA_TYPES is on or the types are known to it")
	}
	return c, nil
}

// Ping implements provider.Pinger: GET /v2/ answered 200, after the auth client answered a
// challenge with the configured credentials. Errors wrap the sentinels of package provider.
func (p *Provider) Ping(ctx context.Context) error {
	_, err := p.ping(ctx)
	return p.translate("ping", err)
}

var _ provider.Pinger = (*Provider)(nil)

// ping sends GET /v2/, as oras-go's Registry.Ping does, and returns what the registry says
// about itself; challengeTransport keeps the challenge of a 401 that the auth client answers.
func (p *Provider) ping(ctx context.Context) (identity, error) {
	id := identity{host: p.host}
	resp, err := p.get(context.WithValue(ctx, challengeKey{}, &id.challenge), p.base+"/v2/")
	if err != nil {
		return id, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		id.header = resp.Header
		return id, nil
	case http.StatusNotFound:
		return id, errdef.ErrNotFound
	}
	return id, parseErrorResponse(resp)
}

// vendorOf names the vendor of the registry that id describes, as detectVendor does, and asks
// a registry that answers GET /v2/ as Gitea does whether it is Forgejo (isForgejo).
func (p *Provider) vendorOf(ctx context.Context, id identity) vendor {
	v := detectVendor(id)
	if v == vendorGitea && p.isForgejo(ctx) {
		return vendorForgejo
	}
	return v
}

// isForgejo reports whether the registry, which answers GET /v2/ as Gitea does, is Forgejo: GET
// of forgejoVersionPath is 200 with a JSON object that names a version. Anything else, an error
// too, leaves it Gitea, whose entry of the vendor table is the same.
func (p *Provider) isForgejo(ctx context.Context) bool {
	resp, err := p.get(ctx, p.base+forgejoVersionPath)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var v struct {
		Version string `json:"version"`
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxForgejoVersionBody)).Decode(&v) == nil && v.Version != ""
}

// probe asks the registry, which answered the ping, what it supports, and applies d, the
// vendor table's capabilities; see Capabilities.
func (p *Provider) probe(ctx context.Context, d config.CapabilityOverrides) (provider.Capabilities, error) {
	c := provider.Capabilities{CreateOnPush: true, DeleteManifest: true, BlobRedirect: true}
	var err error
	if p.override.Catalog == nil && d.Catalog == nil {
		if c.Catalog, err = p.probeCatalog(ctx); err != nil {
			return provider.Capabilities{}, p.translate("probe catalog of "+p.name, err)
		}
	}
	state := referrersUnknown
	switch {
	case p.override.Referrers != nil:
		// The configuration decides; referrersCapability reads it.
	case d.Referrers != nil:
		state = referrersUnsupported
		if *d.Referrers {
			state = referrersSupported
		}
	default:
		if state, err = p.probeReferrers(ctx); err != nil {
			return provider.Capabilities{}, p.translate("probe referrers of "+p.name, err)
		}
	}
	c.Referrers = state == referrersSupported
	p.referrers.Store(state)
	apply(&c, d)
	return c, nil
}

// probeCatalog asks for one entry of the catalog, with the credentials as ListRepositories asks
// (catalogClient): an anonymous 200 is not the catalog of the credentials (#802). A 200 counts
// only with a catalog in its body, a JSON object with repositories: JFrog's router answers 200
// with an empty body, which ListRepositories cannot read.
func (p *Provider) probeCatalog(ctx context.Context) (bool, error) {
	ctx = auth.AppendScopesForHost(ctx, p.host, auth.ScopeRegistryCatalog)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/v2/_catalog?n=1", nil)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	resp, err := catalogClient{p: p}.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		var body struct {
			Repositories json.RawMessage `json:"repositories"`
		}
		err := json.NewDecoder(io.LimitReader(resp.Body, maxCatalogProbeBody)).Decode(&body)
		return err == nil && body.Repositories != nil, nil
	}
	errResp := parseErrorResponse(resp)
	if _, missing := catalogMissing(errResp); missing {
		return false, nil
	}
	return false, errResp
}

// probeReferrers asks for the referrers of the zero digest in the probe repository.
func (p *Provider) probeReferrers(ctx context.Context) (int32, error) {
	ref := registry.Reference{Registry: p.host, Repository: p.repoName(probeRepository), Reference: zeroDigest}
	ctx = auth.AppendRepositoryScope(ctx, ref, auth.ActionPull)
	resp, err := p.get(ctx, p.base+"/v2/"+ref.Repository+"/referrers/"+ref.Reference)
	if err != nil {
		return referrersUnknown, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		if resp.Header.Get("Content-Type") == ocispec.MediaTypeImageIndex {
			return referrersSupported, nil
		}
		return referrersUnsupported, nil
	case http.StatusNotFound:
		if hasCode(parseErrorResponse(resp), errcode.ErrorCodeNameUnknown) {
			return referrersUnknown, nil
		}
		return referrersUnsupported, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return referrersUnknown, nil
	default:
		return referrersUnknown, parseErrorResponse(resp)
	}
}

// consistencyText is the consistency of c as the startup line says it: strong, eventual (with
// its bound when one is known) or unknown.
func consistencyText(c provider.Capabilities) string {
	switch {
	case c.Consistency == provider.ConsistencyUnknown:
		return "unknown"
	case c.ConsistencyBound > 0:
		return string(c.Consistency) + " within " + c.ConsistencyBound.String()
	}
	return string(c.Consistency)
}

// get sends a GET through the client that follows redirects. The caller closes the body.
func (p *Provider) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	return p.clients.follow.Do(req)
}

// catalogMissing reports whether err is a registry's answer that it has no catalog for
// Lajstrom: 401, 403, 404 or 405.
func catalogMissing(err error) (*errcode.ErrorResponse, bool) {
	var resp *errcode.ErrorResponse
	if !errors.As(err, &resp) {
		return nil, false
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed:
		return resp, true
	}
	return nil, false
}

// catalogUsable reports whether ListRepositories may ask for the catalog: not when the
// configuration, the probe or, before a probe, the vendor table for the host says there is
// none.
func (p *Provider) catalogUsable() bool {
	if o := p.override.Catalog; o != nil {
		return *o
	}
	if c := p.probed.Load(); c != nil {
		return c.Catalog
	}
	if d := vendorDefaults[detectVendor(identity{host: p.host})].Catalog; d != nil {
		return *d
	}
	return true
}

// referrersCapability returns whether the repositories should use the referrers API, and
// whether that is known: from the configuration, else from a probe that told.
func (p *Provider) referrersCapability() (capable, known bool) {
	if o := p.override.Referrers; o != nil {
		return *o, true
	}
	switch p.referrers.Load() {
	case referrersSupported:
		return true, true
	case referrersUnsupported:
		return false, true
	}
	return false, false
}
