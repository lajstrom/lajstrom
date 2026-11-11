package generic

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// manifestAccept is the Accept of StatManifest: every manifest media type, as oras-go asks.
var manifestAccept = strings.Join(slices.Sorted(maps.Keys(manifestMediaTypes)), ", ")

// StatManifest sends one HEAD of the manifest d by its digest, redirects followed, and returns
// the descriptor of the answer: Content-Type, Content-Length (-1 without one) and d; a
// Docker-Content-Digest other than d is an error that wraps content.ErrMismatchedDigest. A 404
// is asked again with a GET, since the answer to a HEAD has no body to carry an error code: a
// GET that answers 404 with MANIFEST_UNKNOWN is the one error that wraps
// provider.ErrManifestUnknown (manifestUnknown), and a GET that answers 200 is taken as the
// HEAD would have been. Every other answer goes through translate.
func (p *Provider) StatManifest(ctx context.Context, name string, d digest.Digest) (ocispec.Descriptor, error) {
	op := "stat manifest " + name + "@" + d.String()
	resp, err := p.manifestRequest(ctx, op, http.MethodHead, name, d)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		if resp, err = p.manifestRequest(ctx, op, http.MethodGet, name, d); err != nil {
			return ocispec.Descriptor{}, err
		}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ocispec.Descriptor{}, p.manifestUnknown(op, parseErrorResponse(resp))
	}
	desc := ocispec.Descriptor{MediaType: resp.Header.Get("Content-Type"), Digest: d, Size: resp.ContentLength}
	if v := resp.Header.Get("Docker-Content-Digest"); v != "" && v != d.String() {
		return ocispec.Descriptor{}, fmt.Errorf("generic: %s: the backend answered with the manifest %q: %w", op, v, content.ErrMismatchedDigest)
	}
	return desc, nil
}

// manifestRequest sends one request of the manifest d by its digest through the client that
// follows redirects, and returns the response open, whatever its status.
func (p *Provider) manifestRequest(ctx context.Context, op, method, name string, d digest.Digest) (*http.Response, error) {
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("generic: %s: %w", op, err)
	}
	ref := registry.Reference{Registry: p.host, Repository: p.repoName(name), Reference: d.String()}
	if err := ref.ValidateRepository(); err != nil {
		return nil, p.translate(op, err)
	}
	ctx = auth.AppendRepositoryScope(ctx, ref, auth.ActionPull)
	req, err := http.NewRequestWithContext(ctx, method, p.base+"/v2/"+ref.Repository+"/manifests/"+ref.Reference, nil)
	if err != nil {
		return nil, fmt.Errorf("generic: %s: %w", op, err)
	}
	req.Header.Set("Accept", manifestAccept)
	resp, err := p.clients.follow.Do(req)
	if err != nil {
		return nil, p.translate(op, err)
	}
	return resp, nil
}

// manifestUnknown is Provider.translate for the answer to StatManifest, and marks a 404 with
// MANIFEST_UNKNOWN as provider.ErrManifestUnknown as well.
func (p *Provider) manifestUnknown(op string, resp *errcode.ErrorResponse) error {
	err := p.translate(op, resp)
	var be *backendError
	if errors.As(err, &be) && resp.StatusCode == http.StatusNotFound && hasCode(resp, errcode.ErrorCodeManifestUnknown) {
		be.also = provider.ErrManifestUnknown
	}
	return err
}
