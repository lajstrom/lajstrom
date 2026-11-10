package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// npm has proxy repos: the requests and the parse of the upstream's packuments are in proxy.go.
var _ format.Proxier = Plugin{}

// abbreviatedRoot are the members of the abbreviated packument.
var abbreviatedRoot = []string{"name", "modified", "dist-tags", "versions"}

// RenderProxy renders the packument of kind of name for a proxy repo (docs/spec/
// proxy-and-group.md §7). From kept, the upstream's packument: its members and its dist-tags as
// they are, its versions that listable takes, each with dist.tarball under base, the dist of
// the bytes the repo holds (integrity, shasum) for a version it has, and without the
// attestations, a link to the upstream; a version the repo holds and kept does not list is not
// listed. The abbreviated kind keeps the members of the abbreviated form only. Without kept, the
// packuments of the versions the repo holds, as a hosted repo's (RenderIndex).
func (p Plugin) RenderProxy(ctx context.Context, repo index.Repo, name, kind string, kept []byte, st index.Reader, base string) (format.Document, error) {
	contentType, err := contentTypeOf(kind)
	if err != nil {
		return format.Document{}, err
	}
	if kept == nil {
		return p.renderHeld(ctx, repo, name, kind, st, base)
	}
	var members, listed map[string]json.RawMessage
	if err := json.Unmarshal(kept, &members); err != nil {
		return format.Document{}, fmt.Errorf("npm: the kept packument of %s: %w", name, err)
	}
	if members == nil {
		return format.Document{}, fmt.Errorf("npm: the kept packument of %s is null", name)
	}
	if raw, ok := members["versions"]; ok {
		if err := json.Unmarshal(raw, &listed); err != nil {
			return format.Document{}, fmt.Errorf("npm: the versions of the kept packument of %s: %w", name, err)
		}
	}
	held, err := heldTarballs(ctx, st, repo, name)
	if err != nil {
		return format.Document{}, err
	}
	versions := make(map[string]json.RawMessage, len(listed))
	for v, raw := range listed {
		var vd map[string]json.RawMessage
		if !listable(v, raw) || json.Unmarshal(raw, &vd) != nil {
			continue
		}
		if kind == kindAbbreviated {
			vd = pick(vd, abbreviatedKeys)
		}
		h, ok := held[v]
		if !ok {
			h = heldTarball{file: Filename(name, v)}
		}
		vd["dist"] = proxyDist(vd["dist"], base+name+"/-/"+h.file, h)
		versions[v] = mustJSON(vd)
	}
	members["versions"] = mustJSON(versions)
	if kind == kindAbbreviated {
		members = pick(members, abbreviatedRoot)
	}
	content, err := json.Marshal(members)
	if err != nil {
		return format.Document{}, fmt.Errorf("npm: the proxied packument of %s: %w", name, err)
	}
	return format.Document{Kind: kind, Path: name, ContentType: contentType, Content: content}, nil
}

// renderHeld renders the packument of kind of what the repo holds of name, as RenderIndex does
// for a hosted repo, with the links under base.
func (p Plugin) renderHeld(ctx context.Context, repo index.Repo, name, kind string, st index.Reader, base string) (format.Document, error) {
	p.ExternalURL = strings.TrimSuffix(base, Name+"/"+repo.Name+"/")
	docs, err := p.RenderIndex(ctx, repo, format.IndexScope(name), st)
	if err != nil {
		return format.Document{}, err
	}
	for _, d := range docs {
		if d.Kind == kind {
			return d, nil
		}
	}
	return format.Document{}, fmt.Errorf("%w: %s/%s holds no version of %s", index.ErrNotFound, repo.Format, repo.Name, name)
}

// heldTarball is the tarball of a version the repo holds: its file name and npm's checksums.
type heldTarball struct {
	file, integrity, shasum string
}

// heldTarballs returns the tarballs of the versions of name the repo holds, by version.
func heldTarballs(ctx context.Context, st index.Reader, repo index.Repo, name string) (map[string]heldTarball, error) {
	pkg, err := st.GetPackage(ctx, repo.ID, name)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	versions, err := st.ListVersions(ctx, pkg.ID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]heldTarball, len(versions))
	for _, v := range versions {
		files, err := st.ListFiles(ctx, v.ID)
		if err != nil {
			return nil, err
		}
		if len(files) > 0 {
			f := files[0]
			out[v.Version] = heldTarball{file: f.Name, integrity: f.Attrs[profile.AnnotationNpmIntegrity],
				shasum: f.Attrs[profile.AnnotationNpmShasum]}
		}
	}
	return out, nil
}

// proxyDist returns the upstream's dist of a version with tarball the link, the checksums of the
// held tarball when there is one, and without attestations.
func proxyDist(raw json.RawMessage, link string, h heldTarball) json.RawMessage {
	var dist map[string]json.RawMessage
	if json.Unmarshal(raw, &dist) != nil || dist == nil {
		dist = map[string]json.RawMessage{}
	}
	delete(dist, "attestations")
	dist["tarball"] = mustJSON(link)
	if h.integrity != "" {
		dist["integrity"], dist["shasum"] = mustJSON(h.integrity), mustJSON(h.shasum)
	}
	return mustJSON(dist)
}

// pick returns the members of m that keys names.
func pick(m map[string]json.RawMessage, keys []string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(keys))
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}
