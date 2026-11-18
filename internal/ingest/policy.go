package ingest

import (
	"context"
	"errors"

	"github.com/lajstrom/lajstrom/internal/policy"
)

// ErrDenied is wrapped by a publish that the policy refuses (docs/spec/policy.md §6.3): a
// *policy.DeniedError, which frontends answer 403 and the facade DENIED.
var ErrDenied = policy.ErrDenied

// decide asks the policy about the manifest of pub once it is validated and before anything is
// claimed or written: its coordinates and digest, and for a proxy's fetch the upstream's publish
// time as the quarantine's clock. A refusal is returned as the *policy.DeniedError itself, so
// that its message is the client's, after its audit row policy.refuse (surface upload, or fetch
// for the proxy's). Inspect, the path of reindex, decides nothing: the registry has the manifest.
// The repo's required attachments are left out (docs/spec/policy.md §6.3): a version has none
// before it exists.
func (p *Pipeline) decide(ctx context.Context, pub *publish) error {
	if p.policy == nil || pub.readOnly {
		return nil
	}
	a := pub.parsed.Annotations
	s := policy.Subject{Repo: pub.repo, Name: a.Name, Version: a.Version, Variant: a.Variant, Digest: pub.digest, NoRequirements: true}
	surface := policy.SurfaceUpload
	if pub.origin != nil {
		s.Published, surface = pub.origin.Published, policy.SurfaceFetch
	}
	err := p.policy.Check(ctx, s)
	var denied *policy.DeniedError
	if errors.As(err, &denied) {
		p.policy.Refuse(ctx, surface, denied, pub.principal)
	}
	return err
}
