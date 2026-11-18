package facade

import (
	"context"
	"errors"
	"net/http"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/policy"
)

// deniedError is the answer to a refusal of the policy: 403 DENIED with the reason in the
// message, and in detail with the rule, the verdict or the requirement that decided
// (docs/spec/policy.md §6.5).
func deniedError(denied *policy.DeniedError) *apiError {
	v := denied.Verdict
	detail := map[string]any{"reason": v.Reason}
	if v.Rule != 0 {
		detail["rule"] = v.Rule
	}
	if v.Attestation != "" {
		detail["verdict"] = v.Attestation.String()
	}
	if v.Requirement != nil {
		detail["requirement"] = v.Requirement
	}
	return &apiError{status: http.StatusForbidden, code: codeDenied, msg: denied.Error(), detail: detail, err: denied}
}

// refuse is the answer to an error of the policy: deniedError of a refusal, after its audit row
// policy.refuse on surface; any other error as it is.
func (h *handler) refuse(ctx context.Context, surface string, err error) error {
	var denied *policy.DeniedError
	if !errors.As(err, &denied) {
		return err
	}
	h.o.Policy.Refuse(ctx, surface, denied, auth.PrincipalFrom(ctx))
	return deniedError(denied)
}

// checkPull decides the manifest d that ref resolved to on t, when its repo has
// policy.enforcePull (docs/spec/policy.md §6.5): on a managed repo the version d is, when it is
// one of the package at t's path, else the path, the tag of ref ("" for a digest) and d. The
// repo's required attachments are left out for a manifest that is no version, and for a
// producer's read (producer).
func (h *handler) checkPull(ctx context.Context, t *target, ref string, d digest.Digest) error {
	if h.o.Policy == nil || !policy.EnforcePull(t.repo) {
		return nil
	}
	s, err := h.subjectOf(ctx, t, ref, d)
	if err != nil {
		return err
	}
	s.NoRequirements = s.NoRequirements || producer(ctx, t)
	return h.refuse(ctx, policy.SurfacePull, h.o.Policy.Check(ctx, s))
}

// checkPush decides the manifest d pushed by ref to t, an oci repo, before it passes through
// (docs/spec/policy.md §6.3); a managed repo's push is decided by ingest.
func (h *handler) checkPush(ctx context.Context, t *target, ref string, d digest.Digest) error {
	if h.o.Policy == nil {
		return nil
	}
	s, err := h.subjectOf(ctx, t, ref, d)
	if err != nil {
		return err
	}
	s.NoRequirements = true // a push comes before its attachments (policy.md §6.3)
	return h.refuse(ctx, policy.SurfacePush, h.o.Policy.Check(ctx, s))
}

// subjectOf is the policy subject of the manifest d at t, which ref names. A manifest that is no
// version of the repo (a referrer, a manifest under cosign's legacy tags, any manifest of an oci
// repo) leaves the requirements out (docs/spec/policy.md §6.5).
func (h *handler) subjectOf(ctx context.Context, t *target, ref string, d digest.Digest) (policy.Subject, error) {
	tag := ref
	if _, err := digest.Parse(ref); err == nil {
		tag = ""
	}
	s := policy.Subject{Repo: t.repo, Name: t.path, Version: tag, Digest: d, NoRequirements: true}
	if t.repo.Format == ociFormat {
		return s, nil
	}
	pkg, err := index.PackageOf(ctx, h.o.Index, t.repo.ID, t.path)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return s, nil
	case err != nil:
		return s, err
	}
	v, err := h.o.Index.GetVersionByDigest(ctx, t.repo.ID, d)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return s, nil
	case err != nil:
		return s, err
	case v.PackageID != pkg.ID:
		return s, nil
	}
	return policy.Subject{Repo: t.repo, Name: pkg.Name, Version: v.Version, Variant: v.Variant, Digest: d, Published: v.CreatedAt}, nil
}

// producer reports whether the request's bearer token grants t's repository push or attach
// (docs/spec/rbac.md §4): a producer's, whose read of a manifest is decided without the repo's
// required attachments (docs/spec/policy.md §6.5), as cosign sign, cosign attest and oras attach
// read the version they attach to. A client that only pulls asks for pull; a request without a
// bearer token is no producer's.
func producer(ctx context.Context, t *target) bool {
	g, ok := grantFrom(ctx)
	return ok && g.AllowsAction(t.name, auth.ActionAttach)
}
