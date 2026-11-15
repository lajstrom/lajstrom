package pypi

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The proxy hooks of pypi (ADR 0013, docs/spec/proxy-and-group.md §10, http-routing.md, pypi,
// Proxy): a proxy repo of pypi keeps the files of a project's page on its upstream, a simple
// index in the JSON form of PEP 691 or the HTML form of PEP 503, in proxy_meta as a page of the
// JSON form, under the one kind simple.json; the HTML form a client asks for is that page
// converted (proxyroutes.go). A file is fetched on its first request. RenderProxy is in
// proxyrender.go.

// acceptUpstream is the Accept header of the request for a project's page: the JSON form of
// PEP 691 first, then its HTML form, then text/html, which an upstream older than PEP 691
// answers.
const acceptUpstream = contentTypeJSON + ", " + contentTypeHTML + ";q=0.2, text/html;q=0.01"

// keptAPIVersion is meta.api-version of a page as proxy_meta keeps it: size and upload-time are
// there when the upstream gave them, which 1.0 allows.
const keptAPIVersion = "1.0"

// Key returns the PEP 503 normalized name: My_Pkg is my-pkg. A name that is not a project name
// wraps format.ErrInvalid.
func (Plugin) Key(name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("%w: %q is not a project name", format.ErrInvalid, name)
	}
	return Normalize(name), nil
}

// checkKind refuses a kind other than simple.json, the one kind of a proxy's page: another wraps
// format.ErrInvalid.
func checkKind(kind string) error {
	if kind != kindJSON {
		return fmt.Errorf("%w: a pypi proxy has no document of kind %q", format.ErrInvalid, kind)
	}
	return nil
}

// MetadataRequest is the GET of the project's page below the upstream's URL, the root of its
// simple index as pip's --index-url names it: <normalized name>/, with acceptUpstream.
func (p Plugin) MetadataRequest(name, kind string) (format.UpstreamRequest, error) {
	project, err := p.Key(name)
	if err != nil {
		return format.UpstreamRequest{}, err
	}
	if err := checkKind(kind); err != nil {
		return format.UpstreamRequest{}, err
	}
	return format.UpstreamRequest{Path: url.PathEscape(project) + "/", Accept: acceptUpstream}, nil
}

// upstreamPage is a project's page in the JSON form (PEP 691, PEP 700), as an upstream sends it
// and as proxy_meta keeps it. meta is not read: pypi.org's has _last-serial, a number.
type upstreamPage struct {
	Meta  map[string]any `json:"meta"`
	Name  string         `json:"name"`
	Files []upstreamFile `json:"files"`
}

// upstreamFile is a file of an upstreamPage. As proxy_meta keeps it, URL is as the upstream
// wrote it, without its fragment: absolute, or relative to the project's page; Hashes has the
// sha256 alone; CoreMetadata is {"sha256": …} on a wheel whose core metadata file the upstream
// announced with its sha256 (PEP 658, PEP 714), else absent; Yanked is true or the reason when
// the file is yanked (PEP 592), else absent.
type upstreamFile struct {
	Filename       string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes"`
	RequiresPython string            `json:"requires-python,omitempty"`
	CoreMetadata   json.RawMessage   `json:"core-metadata,omitempty"`
	DistInfo       json.RawMessage   `json:"dist-info-metadata,omitempty"`
	Yanked         json.RawMessage   `json:"yanked,omitempty"`
	Size           *int64            `json:"size,omitempty"`
	UploadTime     string            `json:"upload-time,omitempty"`
}

// ParseMetadata reads a project's page as an upstream sent it, the JSON form when it is a JSON
// object and the HTML form otherwise, or as an earlier parse kept it, and keeps the files a
// proxy serves (listable) in the JSON form. Each is an entry: its version and variant as its
// file name says, the file at its URL (one relative to the page made relative to the upstream's
// URL), its sha256, and its upload-time as the publish time. A JSON page of another project, or
// one that does not decode, wraps format.ErrInvalid.
func (p Plugin) ParseMetadata(name, kind string, body []byte) (*format.UpstreamIndex, error) {
	project, err := p.Key(name)
	if err != nil {
		return nil, err
	}
	if err := checkKind(kind); err != nil {
		return nil, err
	}
	var files []upstreamFile
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '{' {
		var page upstreamPage
		if err := json.Unmarshal(trimmed, &page); err != nil {
			return nil, fmt.Errorf("%w: the page of %s: %w", format.ErrInvalid, project, err)
		}
		if Normalize(page.Name) != project {
			return nil, fmt.Errorf("%w: the page of %s names %q", format.ErrInvalid, project, page.Name)
		}
		files = page.Files
	} else {
		files = htmlFiles(string(body))
	}
	kept := listable(project, files)
	keep, err := marshal(upstreamPage{Meta: map[string]any{"api-version": keptAPIVersion}, Name: project, Files: kept})
	if err != nil {
		return nil, err
	}
	out := &format.UpstreamIndex{Keep: keep, ContentType: contentTypeJSON}
	for _, f := range kept {
		d, _ := describe(f.Filename, "") // listable read it
		out.Entries = append(out.Entries, format.UpstreamEntry{Version: d.version, Variant: d.variant,
			Published: uploadTime(f.UploadTime), Files: []format.UpstreamFile{{Name: f.Filename,
				URL: fromBase(project, f.URL), Checksums: map[string]string{"sha256": f.Hashes["sha256"]}}}})
	}
	return out, nil
}

// listable returns the files of a project's page that a proxy lists and fetches, in their order,
// as proxy_meta keeps them (kept): each file name once; and one source distribution per version,
// which its variant holds alone: the first .tar.gz, else the first .zip. The others are left
// out, so that the page lists only what a file request can fetch.
func listable(project string, files []upstreamFile) []upstreamFile {
	type candidate struct {
		f upstreamFile
		d dist
	}
	var candidates []candidate
	names, tarGz := map[string]bool{}, map[string]bool{}
	for _, f := range files {
		k, d, ok := kept(project, f)
		if !ok || names[f.Filename] {
			continue
		}
		names[f.Filename] = true
		candidates = append(candidates, candidate{k, d})
		if strings.HasSuffix(f.Filename, SdistTarGz) {
			tarGz[d.version] = true
		}
	}
	var out []upstreamFile
	sdists := map[string]bool{}
	for _, c := range candidates {
		if c.d.packagetype == PackagetypeSdist {
			if sdists[c.d.version] || (tarGz[c.d.version] && !strings.HasSuffix(c.f.Filename, SdistTarGz)) {
				continue
			}
			sdists[c.d.version] = true
		}
		out = append(out, c.f)
	}
	return out
}

// kept returns a file of a project's page as proxy_meta keeps it (upstreamFile), and what its
// file name says; ok is false for a file a proxy does not serve: one that is not a wheel or a
// source distribution the plugin takes (.egg, .exe), of another project, without an http(s) URL
// or without a sha256.
func kept(project string, f upstreamFile) (upstreamFile, dist, bool) {
	d, err := describe(f.Filename, "")
	link, ok := fileLink(f.URL)
	sum := strings.ToLower(f.Hashes["sha256"])
	if raw, hexErr := hex.DecodeString(sum); err != nil || !ok || hexErr != nil || len(raw) != 32 || Normalize(d.name) != project {
		return upstreamFile{}, dist{}, false
	}
	k := upstreamFile{Filename: f.Filename, URL: link, Hashes: map[string]string{"sha256": sum},
		RequiresPython: f.RequiresPython, Yanked: keptYank(f.Yanked)}
	if core := coreHash(f.CoreMetadata, f.DistInfo); core != "" && d.packagetype == PackagetypeWheel {
		k.CoreMetadata = mustJSON(map[string]string{"sha256": core})
	}
	if f.Size != nil && *f.Size >= 0 {
		k.Size = f.Size
	}
	if !uploadTime(f.UploadTime).IsZero() {
		k.UploadTime = f.UploadTime
	}
	return k, d, true
}

// fileLink returns the URL of a file without its fragment, and whether it is one a proxy may
// fetch: a reference that parses, http or https when it has a scheme.
func fileLink(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || raw == "" || (u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
		return "", false
	}
	u.Fragment, u.RawFragment = "", ""
	s := u.String()
	return s, s != ""
}

// fromBase returns the URL of a file relative to the upstream's URL, as the core resolves it:
// an absolute URL or path as it is, a relative one below the project's page, <project>/, the
// path of the page's request (MetadataRequest).
func fromBase(project, link string) string {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "" || u.Host != "" || strings.HasPrefix(u.Path, "/") {
		return link
	}
	return url.PathEscape(project) + "/" + link
}

// coreHash returns the sha256 that core-metadata announces, or else dist-info-metadata, its name
// before PEP 714: {"sha256": …} in the JSON form; "" for true, which announces no hash, and for
// anything else.
func coreHash(core, distInfo json.RawMessage) string {
	for _, raw := range []json.RawMessage{core, distInfo} {
		var hashes map[string]string
		if json.Unmarshal(raw, &hashes) != nil {
			continue
		}
		sum := strings.ToLower(hashes["sha256"])
		if b, err := hex.DecodeString(sum); err == nil && len(b) == 32 {
			return sum
		}
	}
	return ""
}

// keptYank returns the yank of a file as proxy_meta keeps it: true, or the reason, a string
// that is not empty; nil when the file is not yanked (false, null, or no value).
func keptYank(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	switch y := v.(type) {
	case string:
		if y != "" {
			return mustJSON(y)
		}
		return json.RawMessage("true")
	case bool:
		if y {
			return json.RawMessage("true")
		}
	}
	return nil
}

// uploadTime reads the upload-time of PEP 700, an RFC 3339 time; zero for anything else.
func uploadTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// mustJSON marshals a value that cannot fail to marshal.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("pypi: marshal %T: %v", v, err))
	}
	return b
}

// FileRequest maps the path of a distribution below the repo, files/<project>/<version>/
// <filename>, to the file: the project normalized, the version, and the variant its file name
// says, which must be of that project and version. Any other path wraps index.ErrNotFound.
func (Plugin) FileRequest(p string) (format.ArtifactRef, error) {
	missing := fmt.Errorf("%w: pypi: %s names no distribution", index.ErrNotFound, p)
	parts := strings.Split(p, "/")
	if len(parts) != 4 || parts[0] != "files" || !ValidName(parts[1]) {
		return format.ArtifactRef{}, missing
	}
	project, version, filename := Normalize(parts[1]), parts[2], parts[3]
	d, err := describe(filename, "")
	if err != nil || Normalize(d.name) != project || d.version != version {
		return format.ArtifactRef{}, missing
	}
	return format.ArtifactRef{Name: project, Version: version, Variant: d.variant, File: filename, Kind: kindJSON}, nil
}

// filePath is the path FileRequest maps, of a file of project and version.
func filePath(project, version, filename string) string {
	return "files/" + project + "/" + version + "/" + filename
}
