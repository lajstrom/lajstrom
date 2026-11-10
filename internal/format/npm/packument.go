package npm

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// The packuments in index_cache (docs/spec/index-schema.md, npm): scope the package name, two
// kinds rendered together.
const (
	kindPackument   = "packument"
	kindAbbreviated = "packument-abbreviated"
)

// The content types of the two packuments.
const (
	contentTypeJSON        = "application/json"
	contentTypeAbbreviated = "application/vnd.npm.install-v1+json"
)

// timeLayout is how the registry writes the times of a packument: UTC, milliseconds.
const timeLayout = "2006-01-02T15:04:05.000Z"

// rootKeys are the members of the full packument taken from the version latest names, its readme
// from the metadata layer.
var rootKeys = []string{"description", "keywords", "license", "homepage", "repository", "bugs", "author"}

// abbreviatedKeys are the members of a version in the abbreviated packument, the registry API's
// abbreviated form, when the version has them.
var abbreviatedKeys = []string{"name", "version", "deprecated", "dependencies", "optionalDependencies", "devDependencies",
	"peerDependencies", "peerDependenciesMeta", "bundleDependencies", "acceptDependencies", "bin", "directories", "engines",
	"cpu", "os", "libc", "funding", "dist", "_hasShrinkwrap", "hasInstallScript"}

// compare orders two versions by SemVer precedence: the numbers, then a release above its
// pre-releases, then the pre-release identifiers, numeric ones below the others.
func (a semver) compare(b semver) int {
	for i := range a.core {
		if c := cmp.Compare(a.core[i], b.core[i]); c != 0 {
			return c
		}
	}
	if a.pre == nil || b.pre == nil {
		return cmp.Compare(len(b.pre), len(a.pre)) // the one without a pre-release is above
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		var c int
		switch xn, yn := allDigits(x), allDigits(y); {
		case xn && yn:
			c = cmp.Or(cmp.Compare(len(x), len(y)), strings.Compare(x, y)) // no leading zeros
		case xn:
			c = -1
		case yn:
			c = 1
		default:
			c = strings.Compare(x, y)
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a.pre), len(b.pre))
}

// packed is a version as the packuments render it.
type packed struct {
	version string
	semver  semver
	created time.Time
	digest  string
	doc     map[string]json.RawMessage // the version of the full packument
	readme  string
}

// RenderIndex renders the two packuments of the package whose name is scope
// (docs/spec/http-routing.md, npm): the full one, kind packument, and the abbreviated one, kind
// packument-abbreviated, from the package's versions and their metadata layers, with an
// absolute dist.tarball under p.ExternalURL. A package the repo does not have renders nothing,
// which format.CachedDocument answers with index.ErrNotFound. Until the journal of #307 there
// are no dist-tags of a publish: latest is the highest release, else the highest version.
func (p Plugin) RenderIndex(ctx context.Context, repo index.Repo, scope format.IndexScope, st index.Reader) ([]format.Document, error) {
	name := string(scope)
	if CheckName(name) != nil {
		return nil, nil
	}
	pkg, err := st.GetPackage(ctx, repo.ID, name)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	versions, err := st.ListVersions(ctx, pkg.ID)
	if err != nil || len(versions) == 0 {
		return nil, err
	}
	all := make([]packed, 0, len(versions))
	for _, v := range versions {
		pv, err := p.pack(ctx, st, repo, name, v)
		if err != nil {
			return nil, err
		}
		all = append(all, pv)
	}
	slices.SortFunc(all, func(a, b packed) int { return a.semver.compare(b.semver) })
	full, abbreviated, err := packuments(name, all)
	if err != nil {
		return nil, err
	}
	return []format.Document{
		{Kind: kindPackument, Path: name, ContentType: contentTypeJSON, Content: full},
		{Kind: kindAbbreviated, Path: name, ContentType: contentTypeAbbreviated, Content: abbreviated},
	}, nil
}

// pack makes the version v of the package name: its metadata's manifest with name, version, _id,
// dist, _hasShrinkwrap and hasInstallScript, or, for a version without its metadata (a degraded
// row), {name, version, dist} from its tarball layer.
func (p Plugin) pack(ctx context.Context, st index.Reader, repo index.Repo, name string, v index.Version) (packed, error) {
	sv, _ := parseVersion(v.Version) // Validate took it
	pv := packed{version: v.Version, semver: sv, created: v.CreatedAt, digest: v.Digest.String(), doc: map[string]json.RawMessage{}}
	var md metadata
	if v.Metadata == nil || json.Unmarshal(v.Metadata, &md) != nil || json.Unmarshal(md.Manifest, &pv.doc) != nil {
		files, err := st.ListFiles(ctx, v.ID)
		if err != nil {
			return packed{}, err
		}
		pv.doc = map[string]json.RawMessage{}
		md = metadata{Filename: Filename(name, v.Version)}
		if len(files) > 0 {
			md.Filename, md.Integrity, md.Shasum = files[0].Name, files[0].Attrs[profile.AnnotationNpmIntegrity], files[0].Attrs[profile.AnnotationNpmShasum]
		}
	} else {
		pv.readme = md.Readme
		pv.doc["_id"] = mustJSON(name + "@" + v.Version)
		pv.doc["_hasShrinkwrap"] = mustJSON(md.HasShrinkwrap)
		pv.doc["hasInstallScript"] = mustJSON(md.HasInstallScript)
	}
	pv.doc["name"], pv.doc["version"] = mustJSON(name), mustJSON(v.Version)
	pv.doc["dist"] = mustJSON(map[string]string{"tarball": p.tarballURL(repo, name, md.Filename), "shasum": md.Shasum, "integrity": md.Integrity})
	return pv, nil
}

// tarballURL is the absolute URL of a tarball: <ExternalURL>/npm/<repo>/<name>/-/<file>, the /
// of a scoped name as it is. Names and versions are made of characters a URL path keeps.
func (p Plugin) tarballURL(repo index.Repo, name, file string) string {
	return strings.TrimSuffix(p.ExternalURL, "/") + "/" + Name + "/" + repo.Name + "/" + name + "/-/" + file
}

// packuments returns the full and the abbreviated packument of the versions, in SemVer order.
func packuments(name string, all []packed) (full, abbreviated []byte, err error) {
	latest := all[len(all)-1]
	for _, pv := range slices.Backward(all) {
		if pv.semver.pre == nil {
			latest = pv
			break
		}
	}
	tags := map[string]string{"latest": latest.version}
	created, modified := all[0].created, all[0].created
	times := ordered{}
	versions, short := ordered{}, ordered{}
	digests := make([]string, 0, len(all))
	for _, pv := range all {
		created, modified = minTime(created, pv.created), maxTime(modified, pv.created)
		times = append(times, member{pv.version, mustJSON(pv.created.UTC().Format(timeLayout))})
		versions = append(versions, member{pv.version, mustJSON(pv.doc)})
		abbreviated := map[string]json.RawMessage{}
		for _, k := range abbreviatedKeys {
			if v, ok := pv.doc[k]; ok {
				abbreviated[k] = v
			}
		}
		short = append(short, member{pv.version, mustJSON(abbreviated)})
		digests = append(digests, pv.digest+"\n")
	}
	slices.Sort(digests)
	sum := sha256.Sum256([]byte(strings.Join(digests, "")))
	doc := ordered{{"_id", mustJSON(name)}, {"_rev", mustJSON("0-" + hex.EncodeToString(sum[:])[:32])}, {"name", mustJSON(name)},
		{"dist-tags", mustJSON(tags)}, {"versions", mustJSON(versions)},
		{"time", mustJSON(append(ordered{{"created", mustJSON(created.UTC().Format(timeLayout))},
			{"modified", mustJSON(modified.UTC().Format(timeLayout))}}, times...))}}
	for _, k := range rootKeys {
		if v, ok := latest.doc[k]; ok {
			doc = append(doc, member{k, v})
		}
	}
	if latest.readme != "" {
		doc = append(doc, member{"readme", mustJSON(latest.readme)})
	}
	if full, err = json.Marshal(doc); err != nil {
		return nil, nil, fmt.Errorf("npm: the packument of %s: %w", name, err)
	}
	abbreviated, err = json.Marshal(ordered{{"name", mustJSON(name)}, {"modified", mustJSON(modified.UTC().Format(timeLayout))},
		{"dist-tags", mustJSON(tags)}, {"versions", mustJSON(short)}})
	if err != nil {
		return nil, nil, fmt.Errorf("npm: the abbreviated packument of %s: %w", name, err)
	}
	return full, abbreviated, nil
}

// member is a member of an ordered JSON object.
type member struct {
	key   string
	value json.RawMessage
}

// ordered is a JSON object whose members are written in order.
type ordered []member

// MarshalJSON writes the members in order.
func (o ordered) MarshalJSON() ([]byte, error) {
	b := []byte{'{'}
	for i, m := range o {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(append(append(b, mustJSON(m.key)...), ':'), m.value...)
	}
	return append(b, '}'), nil
}

// mustJSON returns v as JSON; v is a value that always marshals (strings, bools, maps of them,
// raw JSON).
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("npm: marshal %T: %v", v, err))
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
