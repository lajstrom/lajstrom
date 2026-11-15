package terraform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// publishResponse is the body of a successful publish.
type publishResponse struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	System    string `json:"system"`
	Version   string `json:"version"`
	Filename  string `json:"filename"`
	Digest    string `json:"digest"` // the manifest digest
	Tag       string `json:"tag"`
}

// publish publishes the request body, one module archive, as the version of the path
// (docs/spec/http-routing.md §3, terraform): PUT /terraform/<repo>/<name>/<system>/<version>,
// which needs write. The name, the system and the version are checked first (400), and so is the
// rule that a module's name is unique without regard to case (checkSpelling, 409), before the
// body is read. The body, bounded by the repo policy's maxFileSize (format.LimitBody, 413 above
// it), streams into the registry through deps.Ingest.UploadStream, which keeps it only when the
// publish goes through (ADR 0014), and passes the inspector on its way, which reads it as
// ReadArchive does (400). Nothing of the body is written to a local disk.
//
// The answer is 201 with Location, the path of the archive route, Docker-Content-Digest, the
// manifest digest, and a JSON body. The same bytes again are the idempotent repeat of ingest, 201
// with the digest of the manifest stored first; other bytes under a version of the module are the
// 409 of ingest, whose tag holds another manifest.
func (h *handlers) publish(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r, auth.ActionWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	m, err := ParseModule(r.PathValue("name"), r.PathValue("system"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v := r.PathValue("version")
	if err := CheckVersion(v); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := checkSpelling(ctx, h.deps.Index, repo, m); err != nil {
		h.fail(w, r, err)
		return
	}
	if h.deps.Ingest == nil {
		h.fail(w, r, errors.New("terraform: no publisher in format.Deps"))
		return
	}
	body, err := format.LimitBody(w, r, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	principal := auth.PrincipalFrom(ctx)
	filename := m.Filename(v)
	pub, err := h.deps.Ingest.UploadStream(ctx, repo, format.StreamUpload{
		Name:   m.PackageName(),
		File:   format.UploadedFile{Name: filename, Size: r.ContentLength, ContentType: r.Header.Get("Content-Type")},
		Body:   body,
		Params: map[string]string{ParamName: m.PackageName(), ParamVersion: v, ParamUploadedBy: principal.String()},
	}, principal)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := publishResponse{Namespace: repo.Name, Name: m.Name, System: m.System, Version: v, Filename: filename,
		Digest: pub.Digest.String(), Tag: pub.Tag}
	hdr := w.Header()
	hdr.Set("Location", "/"+Name+"/"+repo.Name+"/"+m.Name+"/"+m.System+"/"+v+"/"+filename)
	hdr.Set("Docker-Content-Digest", pub.Digest.String())
	hdr.Set("Content-Type", jsonContentType)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// checkSpelling applies the rule that a module's name is unique without regard to case
// (docs/spec/manifest-profile.md §4.1): OCI repository names are lowercase, so VPC/aws and
// vpc/aws are one package path, and the first spelling holds it. A package of the repo at m's
// package path with another name is index.ErrConflict naming it (409). The check runs before the
// body streams, so that a refused publish reads no byte of it; two spellings published at once
// both pass it, and the pipeline settles that race under its claim on the package path (#244).
func checkSpelling(ctx context.Context, st index.Reader, repo index.Repo, m Module) error {
	pkg, err := index.PackageAt(ctx, st, repo.ID, m.packagePath())
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("terraform: the module at %q: %w", m.packagePath(), err)
	case pkg.Name != m.PackageName():
		return fmt.Errorf("%w: the module %s exists as %s; a module's name is unique without regard to case", index.ErrConflict,
			m.PackageName(), pkg.Name)
	}
	return nil
}
