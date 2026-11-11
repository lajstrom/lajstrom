package pypi

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The simple index in index_cache (docs/spec/index-schema.md): the scope simple is the project
// list, simple/<project> the page of a project, each in the two kinds of PEP 691.
const (
	scopeProjects     = "simple"
	scopeProjectsPath = "simple/"
	kindHTML          = "simple.html"
	kindJSON          = "simple.json"
)

// The content types of PEP 691: the JSON form, its HTML form, and text/html, which is that
// HTML form too.
const (
	contentTypeJSON = "application/vnd.pypi.simple.v1+json"
	contentTypeHTML = "application/vnd.pypi.simple.v1+html"
	contentTypeText = "text/html; charset=utf-8"
)

// apiVersion is meta.api-version of the JSON and pypi:repository-version of the HTML: 1.1 of
// PEP 700, with versions, size and upload-time.
const apiVersion = "1.1"

// RenderIndex renders a scope of the simple index from the index database: simple, the project
// list, and simple/<project>, the files of a project (normalized name), each as HTML (PEP 503)
// and JSON (PEP 691). A project that is not in the index renders no document, which
// format.CachedDocument answers with index.ErrNotFound.
func (Plugin) RenderIndex(ctx context.Context, repo index.Repo, scope format.IndexScope, st index.Reader) ([]format.Document, error) {
	if scope == scopeProjects {
		pkgs, err := st.ListPackages(ctx, repo.ID, "")
		if err != nil {
			return nil, err
		}
		return projectList(pkgs)
	}
	project, ok := strings.CutPrefix(string(scope), scopeProjectsPath)
	if !ok || project == "" || project != Normalize(project) {
		return nil, fmt.Errorf("%w: pypi scope %q", index.ErrNotFound, scope)
	}
	pkg, err := st.GetPackage(ctx, repo.ID, project)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	files, versions, err := projectFiles(ctx, st, *pkg)
	if err != nil {
		return nil, err
	}
	return projectPage(project, files, versions)
}

// projectFile is a file of a project page.
type projectFile struct {
	Filename       string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes"`
	RequiresPython string            `json:"requires-python,omitempty"`
	CoreMetadata   any               `json:"core-metadata"`
	DistInfo       any               `json:"dist-info-metadata"`
	Size           int64             `json:"size"`
	UploadTime     string            `json:"upload-time"`
	Yanked         bool              `json:"yanked"`
	coreSHA256     string
}

// projectFiles reads the files of every version of pkg, oldest first, and the distinct
// versions in that order. Versions created in the same second, as the files of one twine
// upload are, go by version and variant, not by the order of their rows, which reindex does not
// keep: the page is a function of the rows, the same after a rebuild. The file of a version
// whose metadata the index left out (degraded, or more than ingest keeps) has no core-metadata,
// so pip downloads the file for it.
func projectFiles(ctx context.Context, st index.Reader, pkg index.Package) ([]projectFile, []string, error) {
	vs, err := st.ListVersions(ctx, pkg.ID)
	if err != nil {
		return nil, nil, err
	}
	slices.SortStableFunc(vs, func(a, b index.Version) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.Version, b.Version), strings.Compare(a.Variant, b.Variant))
	})
	var files []projectFile
	versions, seen := []string{}, map[string]bool{}
	for _, v := range vs {
		if !seen[v.Version] {
			versions, seen[v.Version] = append(versions, v.Version), true
		}
		fs, err := st.ListFiles(ctx, v.ID)
		if err != nil {
			return nil, nil, err
		}
		var md struct {
			RequiresPython string `json:"requires_python"`
			CoreMetadata   string `json:"core_metadata"`
		}
		if v.Metadata != nil {
			_ = json.Unmarshal(v.Metadata, &md) // Validate checked it; a row that does not parse has no extras
		}
		for _, f := range fs {
			pf := projectFile{
				Filename: f.Name, URL: fileURL(pkg.Name, v.Version, f.Name), Hashes: map[string]string{"sha256": f.Digest.Encoded()},
				RequiresPython: md.RequiresPython, CoreMetadata: false, DistInfo: false, Size: f.Size,
				UploadTime: v.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000Z"),
			}
			if md.CoreMetadata != "" {
				sum := sha256.Sum256([]byte(md.CoreMetadata))
				pf.coreSHA256 = hex.EncodeToString(sum[:])
				pf.CoreMetadata = map[string]string{"sha256": pf.coreSHA256}
				pf.DistInfo = pf.CoreMetadata
			}
			files = append(files, pf)
		}
	}
	return files, versions, nil
}

// fileURL is the URL of a file relative to the page of its project,
// /pypi/<repo>/simple/<project>/: ../../files/<project>/<version>/<filename>.
func fileURL(project, version, filename string) string {
	return "../../files/" + url.PathEscape(project) + "/" + url.PathEscape(version) + "/" + url.PathEscape(filename)
}

// projectPage renders the page of a project in both kinds.
func projectPage(project string, files []projectFile, versions []string) ([]format.Document, error) {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html>\n  <head>\n    <meta name=\"pypi:repository-version\" content=\"" + apiVersion + "\">\n")
	b.WriteString("    <title>Links for " + html.EscapeString(project) + "</title>\n  </head>\n  <body>\n")
	b.WriteString("    <h1>Links for " + html.EscapeString(project) + "</h1>\n")
	for _, f := range files {
		b.WriteString(`    <a href="` + html.EscapeString(f.URL) + "#sha256=" + f.Hashes["sha256"] + `"`)
		if f.RequiresPython != "" {
			b.WriteString(` data-requires-python="` + html.EscapeString(f.RequiresPython) + `"`)
		}
		if f.coreSHA256 != "" {
			b.WriteString(` data-dist-info-metadata="sha256=` + f.coreSHA256 + `" data-core-metadata="sha256=` + f.coreSHA256 + `"`)
		}
		b.WriteString(">" + html.EscapeString(f.Filename) + "</a><br/>\n")
	}
	b.WriteString("  </body>\n</html>\n")
	if files == nil {
		files = []projectFile{}
	}
	page := struct {
		Meta     map[string]string `json:"meta"`
		Name     string            `json:"name"`
		Versions []string          `json:"versions"`
		Files    []projectFile     `json:"files"`
	}{map[string]string{"api-version": apiVersion}, project, versions, files}
	j, err := marshal(page)
	if err != nil {
		return nil, err
	}
	return []format.Document{
		{Kind: kindHTML, Path: "simple/" + project + "/", ContentType: contentTypeText, Content: []byte(b.String())},
		{Kind: kindJSON, Path: "simple/" + project + "/", ContentType: contentTypeJSON, Content: j},
	}, nil
}

// projectList renders the project list in both kinds.
func projectList(pkgs []index.Package) ([]format.Document, error) {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html>\n  <head>\n    <meta name=\"pypi:repository-version\" content=\"" + apiVersion + "\">\n")
	b.WriteString("    <title>Simple index</title>\n  </head>\n  <body>\n")
	projects := []map[string]string{}
	for _, p := range pkgs {
		b.WriteString(`    <a href="` + html.EscapeString(url.PathEscape(p.Name)) + `/">` + html.EscapeString(p.Name) + "</a><br/>\n")
		projects = append(projects, map[string]string{"name": p.Name})
	}
	b.WriteString("  </body>\n</html>\n")
	j, err := marshal(map[string]any{"meta": map[string]string{"api-version": apiVersion}, "projects": projects})
	if err != nil {
		return nil, err
	}
	return []format.Document{
		{Kind: kindHTML, Path: "simple/", ContentType: contentTypeText, Content: []byte(b.String())},
		{Kind: kindJSON, Path: "simple/", ContentType: contentTypeJSON, Content: j},
	}, nil
}

// marshal is JSON with a final newline and without the escaping of <, > and & that HTML needs.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("pypi: encode the index: %w", err)
	}
	return buf.Bytes(), nil
}
