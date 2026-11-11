package generic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

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
)

// Capabilities probes the registry on the first call and caches the answer; a probe that
// fails is not cached, so the next call probes again. The vendor table (vendor.go) sets what
// the probe cannot measure, and capabilities: from the configuration replace both. The probe
// is:
//
//   - reachability: GET /v2/; a failure is the error of Capabilities. Its headers, the
//     challenge of a 401 the auth client answered on the way, and the host of the URL name
//     the vendor (detectVendor);
//   - catalog: GET /v2/_catalog?n=1; 200 with a catalog (a JSON object with repositories)
//     is true; a 200 without one, 401, 403, 404 and 405 are false;
//   - referrers: GET /v2/<prefix>lajstrom-probe/referrers/<zero digest>; 200 with an OCI
//     image index is true, any other 200 and a 404 without NAME_UNKNOWN are false, and so is
//     an answer that does not tell (404 NAME_UNKNOWN, 401, 403), in which case oras-go decides
//     per repository on first use;
//   - CreateOnPush, DeleteManifest and BlobRedirect are true, ImmutableTags is false,
//     MaxBlobSize is 0 (unknown) and Consistency unknown without a request; DeleteManifest
//     turns false when the registry answers a manifest delete with 405 UNSUPPORTED or a 405
//     without DENIED.
//
// A capability that the vendor table or the configuration sets is not probed. What the vendor
// table decides from the probe's answer (afterProbe) comes before the configuration's overrides,
// with the vendor's quirks: the blob reads of Nexus's oci format and JFrog's blob HEAD. The
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
	if o.MaxBlobSize > 0 {
		c.MaxBlobSize = o.MaxBlobSize
	}
	if o.Consistency != "" {
		c.Consistency = provider.Consistency(o.Consistency)
	}
	if o.ConsistencyBound > 0 {
		c.ConsistencyBound = o.ConsistencyBound
	}
}

// probeOnce returns the cached probe, or probes and caches a successful answer. Concurrent
// first calls wait for one probe.
func (p *Provider) probeOnce(ctx context.Context) (provider.Capabilities, error) {
	if c := p.probed.Load(); c != nil {
		return *c, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.probed.Load(); c != nil {
		return *c, nil
	}
	id, err := p.ping(ctx)
	if err != nil {
		return provider.Capabilities{}, translate("ping "+p.name, err)
	}
	v := detectVendor(id)
	c, err := p.probe(ctx, vendorDefaults[v])
	if err != nil {
		return provider.Capabilities{}, err
	}
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
	)
	if e.Consistency == provider.ConsistencyEventual && e.ConsistencyBound == 0 {
		p.log.WarnContext(ctx, "the registry is eventually consistent and no bound of its lag is known: every publish waits "+
			"until the registry serves it, and answers 503 past the wait; `lajstrom registry probe` measures the lag, "+
			"registries[].capabilities.consistencyBound records it", "vendor", string(v))
	}
	if q != (quirks{}) {
		p.log.InfoContext(ctx, "registry needs workarounds", "vendor", string(v),
			"blobsReadWhole", q.wholeBlobs, "blobExistsByGET", q.blobExistsByGET)
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
	return translate("ping", err)
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

// probe asks the registry, which answered the ping, what it supports, and applies d, the
// vendor table's capabilities; see Capabilities.
func (p *Provider) probe(ctx context.Context, d config.CapabilityOverrides) (provider.Capabilities, error) {
	c := provider.Capabilities{CreateOnPush: true, DeleteManifest: true, BlobRedirect: true}
	var err error
	if p.override.Catalog == nil && d.Catalog == nil {
		if c.Catalog, err = p.probeCatalog(ctx); err != nil {
			return provider.Capabilities{}, translate("probe catalog of "+p.name, err)
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
			return provider.Capabilities{}, translate("probe referrers of "+p.name, err)
		}
	}
	c.Referrers = state == referrersSupported
	p.referrers.Store(state)
	apply(&c, d)
	return c, nil
}

// probeCatalog asks for one entry of the catalog. A 200 counts only with a catalog in its
// body, a JSON object with repositories: JFrog's router answers 200 with an empty body, which
// ListRepositories cannot read.
func (p *Provider) probeCatalog(ctx context.Context) (bool, error) {
	ctx = auth.AppendScopesForHost(ctx, p.host, auth.ScopeRegistryCatalog)
	resp, err := p.get(ctx, p.base+"/v2/_catalog?n=1")
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
