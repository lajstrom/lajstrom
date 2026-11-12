package npm

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The proxy hooks of npm (ADR 0013, docs/spec/proxy-and-group.md §10): a proxy repo of npm keeps
// the upstream's packuments in proxy_meta, of the two kinds a hosted repo renders, and fetches
// a tarball on its first request. A tarball is listed by the full packument, which has the
// publish times of the versions (time), for the quarantine of ADR 0008; the abbreviated one is
// asked of the upstream only for the clients that ask for it (pnpm). RenderProxy is in
// proxyrender.go.

// acceptAbbreviated is the Accept header of a request for the abbreviated packument, pnpm's;
// the full one is asked with application/json, as npm 10 and 11 ask it (ADR 0012).
const acceptAbbreviated = contentTypeAbbreviated + "; q=1.0, application/json; q=0.8, */*"

// digestSizes are the sizes in bytes of the digests a proxy verifies, by algorithm.
var digestSizes = map[string]int{"sha512": 64, "sha256": 32, "sha1": 20}

// Key returns the name as it is: npm names are spelled one way, the upstream's, so the cache
// and the patterns see them unchanged. A name CheckName refuses wraps format.ErrInvalid.
func (Plugin) Key(name string) (string, error) {
	if err := CheckName(name); err != nil {
		return "", err
	}
	return name, nil
}

// MetadataRequest is the GET of the packument of kind of name below the upstream's URL, as the
// clients send it: the scope's / as %2f, the full packument with Accept: application/json, the
// abbreviated one with pnpm's Accept.
func (Plugin) MetadataRequest(name, kind string) (format.UpstreamRequest, error) {
	if err := CheckName(name); err != nil {
		return format.UpstreamRequest{}, err
	}
	accept, err := contentTypeOf(kind)
	if err != nil {
		return format.UpstreamRequest{}, err
	}
	if kind == kindAbbreviated {
		accept = acceptAbbreviated
	}
	return format.UpstreamRequest{Path: escapedName(name), Accept: accept}, nil
}

// escapedName is a name as a segment of a registry URL: the / of a scoped name as %2f, as npm
// writes it. CheckName leaves nothing else to escape.
func escapedName(name string) string { return strings.Replace(name, "/", "%2f", 1) }

// contentTypeOf returns the content type of a packument kind; another kind wraps
// format.ErrInvalid.
func contentTypeOf(kind string) (string, error) {
	switch kind {
	case kindPackument:
		return contentTypeJSON, nil
	case kindAbbreviated:
		return contentTypeAbbreviated, nil
	}
	return "", fmt.Errorf("%w: npm has no document of kind %q", format.ErrInvalid, kind)
}

// upstreamPackument is what a proxy reads in an upstream's packument, full or abbreviated.
type upstreamPackument struct {
	Name     string                     `json:"name"`
	Versions map[string]json.RawMessage `json:"versions"`
	Time     map[string]json.RawMessage `json:"time"` // the full one's; unpublished is an object
}

// upstreamDist is the dist of a version of an upstream's packument.
type upstreamDist struct {
	Dist struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
		Shasum    string `json:"shasum"`
	} `json:"dist"`
}

// ParseMetadata reads an upstream's packument of name and keeps it whole. Each version that is
// SemVer (CheckVersion) and an object is an entry: one tarball, named as Build names it, at
// dist.tarball (<name>/-/<file> below the upstream's URL without one), with the checksums of
// dist.integrity and dist.shasum, and its time when the packument has one. A body that is not a
// packument of name wraps format.ErrInvalid.
func (Plugin) ParseMetadata(name, kind string, body []byte) (*format.UpstreamIndex, error) {
	contentType, err := contentTypeOf(kind)
	if err != nil {
		return nil, err
	}
	var doc upstreamPackument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%w: the packument of %s: %w", format.ErrInvalid, name, err)
	}
	if doc.Name != name {
		return nil, fmt.Errorf("%w: the packument of %s names %q", format.ErrInvalid, name, doc.Name)
	}
	out := &format.UpstreamIndex{Keep: body, ContentType: contentType}
	for _, v := range slices.Sorted(maps.Keys(doc.Versions)) {
		var vd upstreamDist
		if !listable(v, doc.Versions[v]) || json.Unmarshal(doc.Versions[v], &vd) != nil {
			continue
		}
		file := Filename(name, v)
		link := vd.Dist.Tarball
		if link == "" {
			link = escapedName(name) + "/-/" + file
		}
		out.Entries = append(out.Entries, format.UpstreamEntry{Version: v, Published: published(doc.Time[v]),
			Files: []format.UpstreamFile{{Name: file, URL: link, Checksums: checksums(vd.Dist.Integrity, vd.Dist.Shasum)}}})
	}
	return out, nil
}

// listable reports whether a version of an upstream's packument is one a proxy lists and
// fetches: SemVer as npm publishes it, and an object. Another is left out of the packument, so
// that it lists only what a tarball request can fetch.
func listable(version string, doc json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(doc))
	return CheckVersion(version) == nil && strings.HasPrefix(trimmed, "{")
}

// checksums returns the checksums of a tarball by algorithm, as lowercase hex: those of
// integrity, a Subresource Integrity list of <algorithm>-<base64> separated by spaces (an
// option after ?), the first of an algorithm, and shasum, the sha1 in hex, when integrity has
// none. A token that does not decode to a digest of its algorithm is left out.
func checksums(integrity, shasum string) map[string]string {
	sums := map[string]string{}
	for _, token := range strings.Fields(integrity) {
		alg, b64, _ := strings.Cut(token, "-")
		b64, _, _ = strings.Cut(b64, "?")
		raw, err := base64.StdEncoding.DecodeString(b64)
		if _, seen := sums[alg]; err != nil || seen || len(raw) != digestSizes[alg] {
			continue
		}
		sums[alg] = hex.EncodeToString(raw)
	}
	if raw, err := hex.DecodeString(shasum); err == nil && len(raw) == digestSizes["sha1"] && sums["sha1"] == "" {
		sums["sha1"] = hex.EncodeToString(raw)
	}
	return sums
}

// published reads a time of the packument, zero when it is not an RFC 3339 string.
func published(raw json.RawMessage) time.Time {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// FileRequest maps the path of a tarball, <name>/-/<name without its scope>-<version>.tgz with
// the scope's / as it is or as %2f, to the version it names, listed by the full packument. Any
// other path wraps index.ErrNotFound.
func (Plugin) FileRequest(p string) (format.ArtifactRef, error) {
	missing := fmt.Errorf("%w: npm: %s names no tarball", index.ErrNotFound, p)
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}
	t, ok := parseTarget(p)
	if !ok || len(t.rest) != 2 || t.rest[0] != "-" || CheckName(t.name) != nil {
		return format.ArtifactRef{}, missing
	}
	file := t.rest[1]
	_, base, _ := splitName(t.name)
	v, prefixed := strings.CutPrefix(file, base+"-")
	v, tgz := strings.CutSuffix(v, ".tgz")
	if !prefixed || !tgz || CheckVersion(v) != nil {
		return format.ArtifactRef{}, missing
	}
	return format.ArtifactRef{Name: t.name, Version: v, File: file, Kind: kindPackument}, nil
}

// npm's proxy repos take a prefetch (docs/spec/proxy-and-group.md §5.1).
var _ format.Prefetcher = Plugin{}

// PrefetchKinds are the full packument, which lists every version with its time, and the
// abbreviated one, which npm install and pnpm ask for.
func (Plugin) PrefetchKinds() []string { return []string{kindPackument, kindAbbreviated} }

// Pointed returns the targets of the dist-tags of a packument of name as ParseMetadata kept it,
// sorted, each once; a target that is not a string is left out. A body that is not a JSON
// object, or a kind npm does not have, wraps format.ErrInvalid.
func (Plugin) Pointed(name, kind string, kept []byte) ([]string, error) {
	if _, err := contentTypeOf(kind); err != nil {
		return nil, err
	}
	var doc struct {
		DistTags map[string]json.RawMessage `json:"dist-tags"`
	}
	if err := json.Unmarshal(kept, &doc); err != nil {
		return nil, fmt.Errorf("%w: the packument of %s: %w", format.ErrInvalid, name, err)
	}
	targets := map[string]bool{}
	for _, raw := range doc.DistTags {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			targets[v] = true
		}
	}
	return slices.Sorted(maps.Keys(targets)), nil
}
