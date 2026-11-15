package pypi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// pypi has proxy repos: the request and the parse of the upstream's pages are in proxy.go.
var _ format.Proxier = Plugin{}

// RenderProxy renders the JSON page of a project for a proxy repo (docs/spec/proxy-and-group.md
// §7, http-routing.md, pypi, Proxy). From kept, the upstream's files as ParseMetadata kept them,
// in their order: each with its URL relative to the page, as a hosted repo's, so that it names
// the repo the page is served for and never the upstream; its requires-python, yank, size and
// upload-time; its core-metadata, on a wheel, as the upstream announced it. A file the repo holds
// has the sha256, the size and the core metadata of the index, and its created when the
// upstream gave no upload-time. The API version is 1.1 when every file has its size, else 1.0;
// versions are those of the files, in order. Without kept, the page of the files the repo holds,
// as a hosted repo's. base is not read: the links are relative.
func (p Plugin) RenderProxy(ctx context.Context, repo index.Repo, name, kind string, kept []byte, st index.Reader, _ string) (format.Document, error) {
	project, err := p.Key(name)
	if err != nil {
		return format.Document{}, err
	}
	if err := checkKind(kind); err != nil {
		return format.Document{}, err
	}
	if kept == nil {
		return p.renderHeld(ctx, repo, project, st)
	}
	var page upstreamPage
	if err := json.Unmarshal(kept, &page); err != nil {
		return format.Document{}, fmt.Errorf("pypi: the kept page of %s: %w", project, err)
	}
	held, err := heldFiles(ctx, st, repo, project)
	if err != nil {
		return format.Document{}, err
	}
	version, files, versions, seen := apiVersion, []projectFile{}, []string{}, map[string]bool{}
	for _, f := range page.Files {
		pf, v, ok := proxiedFile(project, f, held)
		if !ok {
			continue
		}
		if pf.Size == nil {
			version = keptAPIVersion
		}
		if !seen[v] {
			versions, seen[v] = append(versions, v), true
		}
		files = append(files, pf)
	}
	content, err := pageJSON(project, version, files, versions)
	if err != nil {
		return format.Document{}, err
	}
	return format.Document{Kind: kindJSON, Path: scopeProjectsPath + project + "/", ContentType: contentTypeJSON, Content: content}, nil
}

// proxiedFile returns a kept file of project as a proxy's page lists it, and its version: the
// index's sha256, size and core metadata when the repo holds it (held). ok is false for a file
// name the plugin does not read.
func proxiedFile(project string, f upstreamFile, held map[string]heldFile) (projectFile, string, bool) {
	d, err := describe(f.Filename, "")
	if err != nil {
		return projectFile{}, "", false
	}
	pf := projectFile{Filename: f.Filename, URL: fileURL(project, d.version, f.Filename), Hashes: map[string]string{"sha256": f.Hashes["sha256"]},
		RequiresPython: f.RequiresPython, CoreMetadata: false, DistInfo: false, Size: f.Size, Yanked: false}
	if t := uploadTime(f.UploadTime); !t.IsZero() {
		pf.UploadTime = t.Format(uploadTimeLayout)
	}
	core := coreHash(f.CoreMetadata, nil)
	if h, ok := held[f.Filename]; ok && h.version == d.version {
		pf.Hashes["sha256"], pf.Size, core = h.sha256, &h.size, h.core
		if pf.UploadTime == "" {
			pf.UploadTime = h.created.UTC().Format(uploadTimeLayout)
		}
	}
	if core != "" && isWheel(f.Filename) {
		pf.coreSHA256, pf.CoreMetadata = core, map[string]string{"sha256": core}
		pf.DistInfo = pf.CoreMetadata
	}
	if reason := yankReason(f.Yanked); reason != nil {
		pf.yanked, pf.Yanked = reason, true
		if *reason != "" {
			pf.Yanked = *reason
		}
	}
	return pf, d.version, true
}

// yankReason reads the yank of a page's file, true or the reason: a pointer to the reason, ""
// for none; nil when the file is not yanked.
func yankReason(raw json.RawMessage) *string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	switch y := v.(type) {
	case string:
		return &y
	case bool:
		if y {
			return new(string)
		}
	}
	return nil
}

// renderHeld renders the JSON page of the files the repo holds of project, as a hosted repo's;
// index.ErrNotFound when it holds none.
func (p Plugin) renderHeld(ctx context.Context, repo index.Repo, project string, st index.Reader) (format.Document, error) {
	docs, err := p.RenderIndex(ctx, repo, format.IndexScope(scopeProjectsPath+project), st)
	if err != nil {
		return format.Document{}, err
	}
	for _, d := range docs {
		if d.Kind == kindJSON {
			return d, nil
		}
	}
	return format.Document{}, fmt.Errorf("%w: %s/%s holds no file of %s", index.ErrNotFound, repo.Format, repo.Name, project)
}

// heldFile is a file of a version the repo holds, as a page says it: the version, the sha256,
// the size, the sha256 of the core metadata the index has ("" for none) and created.
type heldFile struct {
	version, sha256, core string
	size                  int64
	created               time.Time
}

// heldFiles returns the files the repo holds of project, by file name.
func heldFiles(ctx context.Context, st index.Reader, repo index.Repo, project string) (map[string]heldFile, error) {
	pkg, err := st.GetPackage(ctx, repo.ID, project)
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
	out := map[string]heldFile{}
	for _, v := range versions {
		files, err := st.ListFiles(ctx, v.ID)
		if err != nil {
			return nil, err
		}
		var md struct {
			CoreMetadata string `json:"core_metadata"`
		}
		if v.Metadata != nil {
			_ = json.Unmarshal(v.Metadata, &md) // Validate checked it; a row that does not parse has no core metadata
		}
		core := ""
		if md.CoreMetadata != "" {
			sum := sha256.Sum256([]byte(md.CoreMetadata))
			core = hex.EncodeToString(sum[:])
		}
		for _, f := range files {
			out[f.Name] = heldFile{version: v.Version, sha256: f.Digest.Encoded(), core: core, size: f.Size, created: v.CreatedAt}
		}
	}
	return out, nil
}
