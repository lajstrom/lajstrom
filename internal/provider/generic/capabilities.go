package generic

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/lajstrom/lajstrom/internal/provider"
)

const (
	// probeRepository is the repository, below the instance prefix, whose referrers the probe
	// asks for. It need not exist: a registry with the referrers API answers an empty index.
	probeRepository = "lajstrom-probe"
	// zeroDigest is the subject of the referrers probe; no manifest has it.
	zeroDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

// Capabilities probes the registry on the first call and caches the answer; a probe that
// fails is not cached, so the next call probes again. Then capabilities: from the
// configuration replace what the probe found. The probe is:
//
//   - reachability: GET /v2/; a failure is the error of Capabilities;
//   - catalog: GET /v2/_catalog?n=1; 200 is true, 401, 403, 404 and 405 are false;
//   - referrers: GET /v2/<prefix>lajstrom-probe/referrers/<zero digest>; 200 with an OCI
//     image index is true, any other 200 and a 404 without NAME_UNKNOWN are false, and so is
//     an answer that does not tell (404 NAME_UNKNOWN, 401, 403), in which case oras-go decides
//     per repository on first use;
//   - CreateOnPush, DeleteManifest and BlobRedirect are true, ImmutableTags is false and
//     MaxBlobSize is 0 (unknown) without a request; DeleteManifest turns false when the
//     registry answers a manifest delete with 405.
//
// A capability that the configuration sets is not probed. The first successful probe logs
// the result at info.
func (p *Provider) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	c, err := p.probeOnce(ctx)
	if err != nil {
		return provider.Capabilities{}, err
	}
	return p.effective(c), nil
}

// effective applies what Lajstrom learnt since the probe, then the overrides, to probed.
func (p *Provider) effective(probed provider.Capabilities) provider.Capabilities {
	c := probed
	if p.deleteRefused.Load() {
		c.DeleteManifest = false
	}
	o := p.override
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
	return c
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
	c, err := p.probe(ctx)
	if err != nil {
		return provider.Capabilities{}, err
	}
	p.probed.Store(&c)
	e := p.effective(c)
	p.log.InfoContext(ctx, "registry capabilities",
		"createOnPush", e.CreateOnPush,
		"referrers", e.Referrers,
		"catalog", e.Catalog,
		"deleteManifest", e.DeleteManifest,
		"immutableTags", e.ImmutableTags,
		"blobRedirect", e.BlobRedirect,
		"maxBlobSize", e.MaxBlobSize,
	)
	return c, nil
}

// probe asks the registry what it supports; see Capabilities.
func (p *Provider) probe(ctx context.Context) (provider.Capabilities, error) {
	if err := p.registry.Ping(ctx); err != nil {
		return provider.Capabilities{}, translate("ping "+p.name, err)
	}
	c := provider.Capabilities{CreateOnPush: true, DeleteManifest: true, BlobRedirect: true}
	var err error
	if p.override.Catalog == nil {
		if c.Catalog, err = p.probeCatalog(ctx); err != nil {
			return provider.Capabilities{}, translate("probe catalog of "+p.name, err)
		}
	}
	state := referrersUnknown
	if p.override.Referrers == nil {
		if state, err = p.probeReferrers(ctx); err != nil {
			return provider.Capabilities{}, translate("probe referrers of "+p.name, err)
		}
	}
	c.Referrers = state == referrersSupported
	p.referrers.Store(state)
	return c, nil
}

// probeCatalog asks for one entry of the catalog.
func (p *Provider) probeCatalog(ctx context.Context) (bool, error) {
	ctx = auth.AppendScopesForHost(ctx, p.host, auth.ScopeRegistryCatalog)
	resp, err := p.get(ctx, p.base+"/v2/_catalog?n=1")
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return true, nil
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
// configuration or the probe says there is none.
func (p *Provider) catalogUsable() bool {
	if o := p.override.Catalog; o != nil {
		return *o
	}
	c := p.probed.Load()
	return c == nil || c.Catalog
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
