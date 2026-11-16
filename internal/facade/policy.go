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
// message, and in detail with the rule or the verdict that decided (docs/spec/policy.md §6.5).
func deniedError(denied *policy.DeniedError) *apiError {
	v := denied.Verdict
	detail := map[string]any{"reason": v.Reason}
	if v.Rule != 0 {
		detail["rule"] = v.Rule
	}
	if v.Attestation != "" {
		detail["verdict"] = v.Attestation.String()
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
// one of the package at t's path, else the path, the tag of ref ("" for a digest) and d.
func (h *handler) checkPull(ctx context.Context, t *target, ref string, d digest.Digest) error {
	if h.o.Policy == nil || !policy.EnforcePull(t.repo) {
		return nil
	}
	s, err := h.subjectOf(ctx, t, ref, d)
	if err != nil {
		return err
	}
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
	return h.refuse(ctx, policy.SurfacePush, h.o.Policy.Check(ctx, s))
}

// subjectOf is the policy subject of the manifest d at t, which ref names.
func (h *handler) subjectOf(ctx context.Context, t *target, ref string, d digest.Digest) (policy.Subject, error) {
	tag := ref
	if _, err := digest.Parse(ref); err == nil {
		tag = ""
	}
	s := policy.Subject{Repo: t.repo, Name: t.path, Version: tag, Digest: d}
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
