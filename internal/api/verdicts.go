package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/policy"
	"github.com/lajstrom/lajstrom/internal/policy/denylist"
)

// Verdicter attaches the verdicts the server makes and resolves the manifests they judge: the
// ingest pipeline (docs/spec/policy.md §7).
type Verdicter interface {
	AttachVerdict(ctx context.Context, repo index.Repo, path string, subject digest.Digest, v attest.Verdict, key ingest.VerdictSigner,
		principal auth.Principal) (digest.Digest, error)
	ResolveManifest(ctx context.Context, repo index.Repo, path, ref string) (ocispec.Descriptor, error)
	RemoveVerdicts(ctx context.Context, repo index.Repo, path string, subject digest.Digest, source string, principal auth.Principal) (int, error)
}

// PolicyKey is the server's policy key as the API uses it: it signs, and its public key is
// answered (keys.PolicyKey).
type PolicyKey interface {
	ingest.VerdictSigner
	PEM() []byte
}

// VerdictView is a verdict as the API answers it (docs/api.md, Verdicts).
type VerdictView struct {
	Repo        string        `json:"repo"`
	Package     string        `json:"package"`
	Version     string        `json:"version,omitempty"`
	Variant     string        `json:"variant,omitempty"`
	Subject     digest.Digest `json:"subject"`
	Attestation digest.Digest `json:"attestation"`
	Verdict     string        `json:"verdict"`
	Reason      string        `json:"reason"`
	Source      string        `json:"source"`
	Scope       string        `json:"scope"`
	ValidUntil  *time.Time    `json:"validUntil"`
	SignedBy    string        `json:"signedBy"`
	CreatedAt   *time.Time    `json:"createdAt"`
	InForce     bool          `json:"inForce"`
}

// VerdictList is the answer of GET /api/v1/policy/verdicts and of a deny.
type VerdictList struct {
	Items []VerdictView `json:"items"`
}

// listVerdicts answers GET /api/v1/policy/verdicts?digest=&repo=: the verdicts of a subject, of a
// repo or both, of the repos the caller may read, newest first.
func (h *handler) listVerdicts(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	ctx := r.Context()
	q := r.URL.Query()
	var f index.VerdictFilter
	if d := q.Get("digest"); d != "" {
		parsed, err := digest.Parse(d)
		if err != nil {
			return 0, nil, invalid("digest: %q is not a digest", d)
		}
		f.Subject = parsed
	}
	if ref := q.Get("repo"); ref != "" {
		fn, n, ok := strings.Cut(ref, "/")
		if !ok {
			return 0, nil, invalid("repo: %q is not <format>/<repo>", ref)
		}
		if err := h.allow(r, auth.ActionRead, fn, n); err != nil {
			return 0, nil, err
		}
		repo, err := h.repos.Index.GetRepo(ctx, fn, n)
		if err != nil {
			return 0, nil, repoError("list the verdicts of", fn, n, err)
		}
		f.RepoID = repo.ID
	}
	if f.Subject == "" && f.RepoID == 0 {
		return 0, nil, invalid("digest, repo: one of the two is required")
	}
	rows, err := h.repos.Index.ListVerdicts(ctx, f)
	if err != nil {
		return 0, nil, err
	}
	items, err := h.verdictViews(r, rows)
	return http.StatusOK, VerdictList{Items: items}, err
}

// verdictViews returns the views of rows of the repos the caller may read.
func (h *handler) verdictViews(r *http.Request, rows []index.Verdict) ([]VerdictView, error) {
	ctx := r.Context()
	repos := map[int64]*index.Repo{}
	readable := map[int64]bool{}
	now := time.Now()
	out := []VerdictView{}
	for _, v := range rows {
		repo, ok := repos[v.RepoID]
		if !ok {
			got, err := h.repos.Index.GetRepoByID(ctx, v.RepoID)
			if err != nil && !errors.Is(err, index.ErrNotFound) {
				return nil, err
			}
			repo, repos[v.RepoID] = got, got
			readable[v.RepoID] = got != nil && h.allow(r, auth.ActionRead, got.Format, got.Name) == nil
		}
		if !readable[v.RepoID] {
			continue
		}
		view := VerdictView{Repo: repo.Format + "/" + repo.Name, Package: v.Path, Subject: v.Subject, Attestation: v.Referrer,
			Verdict: v.Verdict, Reason: v.Reason, Source: v.Source, Scope: v.Scope, ValidUntil: v.ValidUntil, SignedBy: v.SignedBy,
			CreatedAt: v.CreatedAt, InForce: !v.Expired(now)}
		if v.AttachmentID != 0 {
			if pv, err := h.repos.Index.GetVersionByDigest(ctx, repo.ID, v.Subject); err == nil {
				if pkg, err := index.PackageOf(ctx, h.repos.Index, repo.ID, v.Path); err == nil && pkg.ID == pv.PackageID {
					view.Package, view.Version, view.Variant = pkg.Name, pv.Version, pv.Variant
				}
			}
		}
		out = append(out, view)
	}
	return out, nil
}

// denyBody is the body of POST …/deny.
type denyBody struct {
	Reason     string     `json:"reason"`
	ValidUntil *time.Time `json:"validUntil"`
}

// target is a manifest a deny names: the package path of its OCI repository and its digest.
type denyTarget struct {
	path   string
	digest digest.Digest
}

// denyVersion answers POST …/packages/{name}/versions/{version}/deny[?variant=]: a verdict deny,
// source lajstrom:manual, scope the repo, signed with the policy key, attached to the manifest
// of each variant of the version (docs/spec/policy.md §7); 201 with the verdicts. manage on the
// repo, before the repo is looked up; 501 without the policy key.
func (h *handler) denyVersion(w http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return 0, nil, err
	}
	if h.policyKey == nil || h.verdicter == nil {
		return 0, nil, &clientError{msg: "the server has no policy key to sign a verdict with: set keys.masterKeyEnv or keys.masterKeyFile",
			err: format.ErrUnsupported}
	}
	body, err := readDeny(w, r)
	if err != nil {
		return 0, nil, err
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("deny a version in", f, name, err)
	}
	targets, err := h.denyTargets(r, repo)
	if err != nil {
		return 0, nil, err
	}
	v := attest.Verdict{Verdict: index.PolicyDeny, Reason: body.Reason, Source: policy.SourceManual, Scope: f + "/" + name,
		ValidUntil: body.ValidUntil}
	referrers := make([]digest.Digest, 0, len(targets))
	for _, t := range targets {
		d, err := h.verdicter.AttachVerdict(ctx, *repo, t.path, t.digest, v, h.policyKey, auth.PrincipalFrom(ctx))
		if err != nil {
			return 0, nil, fmt.Errorf("api: the verdict of %s in %s/%s: %w", t.digest, f, name, err)
		}
		referrers = append(referrers, d)
	}
	made, err := h.verdictsOf(ctx, repo, referrers)
	if err != nil {
		return 0, nil, err
	}
	items, err := h.verdictViews(r, made)
	return http.StatusCreated, VerdictList{Items: items}, err
}

// readDeny reads the body of a deny: a reason of 1 to 512 bytes of UTF-8, and a validUntil in
// the future or none.
func readDeny(w http.ResponseWriter, r *http.Request) (denyBody, error) {
	var body denyBody
	if err := decodeBody(w, r, &body); err != nil {
		return body, err
	}
	switch {
	case body.Reason == "" || len(body.Reason) > 512 || !utf8.ValidString(body.Reason):
		return body, invalid("reason: 1 to 512 bytes of UTF-8")
	case body.ValidUntil != nil && !body.ValidUntil.After(time.Now()):
		return body, invalid("validUntil: must be in the future")
	}
	return body, nil
}

// verdictsOf returns the verdicts rows of the referrers of repo.
func (h *handler) verdictsOf(ctx context.Context, repo *index.Repo, referrers []digest.Digest) ([]index.Verdict, error) {
	rows, err := h.repos.Index.ListVerdicts(ctx, index.VerdictFilter{RepoID: repo.ID})
	if err != nil {
		return nil, err
	}
	var made []index.Verdict
	for _, row := range rows {
		if slices.Contains(referrers, row.Referrer) {
			made = append(made, row)
		}
	}
	return made, nil
}

// undenyVersion answers DELETE …/deny[?variant=]: the verdicts of source lajstrom:manual of the
// version's manifests are removed through the Verdicter (their referrers deleted, cosign's legacy
// attestation written again without them); 204, 404 when there is none. manage on the repo.
func (h *handler) undenyVersion(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return 0, nil, err
	}
	if h.verdicter == nil {
		return 0, nil, &clientError{msg: "removing verdicts is not enabled in this server", err: format.ErrUnsupported}
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("remove a deny in", f, name, err)
	}
	targets, err := h.denyTargets(r, repo)
	if err != nil {
		return 0, nil, err
	}
	removed := 0
	for _, t := range targets {
		n, err := h.verdicter.RemoveVerdicts(ctx, *repo, t.path, t.digest, policy.SourceManual, auth.PrincipalFrom(ctx))
		if err != nil {
			return 0, nil, fmt.Errorf("api: remove the verdicts of %s in %s/%s: %w", t.digest, f, name, err)
		}
		removed += n
	}
	if removed == 0 {
		return 0, nil, &clientError{msg: fmt.Sprintf("%s %s has no verdict of source %s", r.PathValue("name"), r.PathValue("version"),
			policy.SourceManual), err: index.ErrNotFound}
	}
	return http.StatusNoContent, nil, nil
}

// denyTargets returns the manifests that a deny of the request names in repo: on a managed repo
// those of the version, each variant or the one of ?variant=; on an oci repo the manifest that
// {version}, a tag or a digest, names at the path {name}.
func (h *handler) denyTargets(r *http.Request, repo *index.Repo) ([]denyTarget, error) {
	ctx := r.Context()
	name, version := r.PathValue("name"), r.PathValue("version")
	if repo.Format == "oci" {
		desc, err := h.verdicter.ResolveManifest(ctx, *repo, name, version)
		if err != nil {
			return nil, &clientError{msg: fmt.Sprintf("oci/%s has no manifest %s:%s", repo.Name, name, version), err: index.ErrNotFound}
		}
		return []denyTarget{{path: name, digest: desc.Digest}}, nil
	}
	var variant *string
	if v, ok := r.URL.Query()["variant"]; ok {
		variant = &v[0]
	}
	pkg, versions, err := h.versionOf(ctx, repo, name, version, variant)
	if err != nil {
		return nil, err
	}
	out := make([]denyTarget, 0, len(versions))
	for _, v := range versions {
		out = append(out, denyTarget{path: index.ManifestPath(*pkg, v), digest: v.Digest})
	}
	return out, nil
}

// policyKeyPEM answers GET /api/v1/policy/key: the policy key's public key, PEM, to anyone; 404
// without one.
func (h *handler) policyKeyPEM(w http.ResponseWriter, _ *http.Request) (int, any, error) {
	if h.policyKey == nil {
		return 0, nil, &clientError{msg: "the server has no policy key", err: index.ErrNotFound}
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.policyKey.PEM())
	return 0, nil, nil
}

// denylist answers GET /api/v1/policy/denylist: the deny list of docs/spec/policy.md §8, read from
// the index now, with the entries of the repos the caller may read.
func (h *handler) denylist(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	doc, err := denylist.Build(r.Context(), h.repos.Index, func(repo index.Repo) bool {
		return h.allow(r, auth.ActionRead, repo.Format, repo.Name) == nil
	}, h.locate)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, doc, nil
}
