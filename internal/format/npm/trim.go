package npm

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/lajstrom/lajstrom/internal/format"
)

var _ format.Trimmer = Plugin{}

// Trim returns the kept packument of name without the versions drop names (format.Trimmer, the
// versions a proxy's policy refuses): out of versions and time, and out of dist-tags, where
// latest then names the greatest version left that is not a prerelease, else the greatest one
// left, and is absent when none is left. Every other member stays as the upstream sent it.
func (Plugin) Trim(name, _ string, kept []byte, drop func(version, variant string) bool) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(kept, &doc); err != nil {
		return nil, fmt.Errorf("%w: the kept packument of %s: %w", format.ErrInvalid, name, err)
	}
	var versions map[string]json.RawMessage
	if raw, ok := doc["versions"]; ok && json.Unmarshal(raw, &versions) != nil {
		return nil, fmt.Errorf("%w: the versions of the kept packument of %s", format.ErrInvalid, name)
	}
	dropped := map[string]bool{}
	for v := range versions {
		if drop(v, "") {
			dropped[v] = true
			delete(versions, v)
		}
	}
	if len(dropped) == 0 {
		return kept, nil
	}
	doc["versions"] = mustJSON(versions)
	trimTime(doc, dropped)
	trimTags(doc, dropped, versions)
	return json.Marshal(doc)
}

// trimTime leaves the versions dropped out of the time of doc.
func trimTime(doc map[string]json.RawMessage, dropped map[string]bool) {
	raw, ok := doc["time"]
	var times map[string]json.RawMessage
	if !ok || json.Unmarshal(raw, &times) != nil {
		return
	}
	for v := range dropped {
		delete(times, v)
	}
	doc["time"] = mustJSON(times)
}

// trimTags leaves the dist-tags of doc that name a version dropped out, and moves latest, when
// it went, to the greatest of versions.
func trimTags(doc map[string]json.RawMessage, dropped map[string]bool, versions map[string]json.RawMessage) {
	raw, ok := doc["dist-tags"]
	var tags map[string]string
	if !ok || json.Unmarshal(raw, &tags) != nil {
		return
	}
	for tag, v := range tags {
		if dropped[v] {
			delete(tags, tag)
		}
	}
	if _, ok := tags["latest"]; !ok {
		if latest, ok := greatest(slices.Collect(maps.Keys(versions))); ok {
			tags["latest"] = latest
		}
	}
	doc["dist-tags"] = mustJSON(tags)
}

// greatest returns the greatest of versions that is not a prerelease, else the greatest one,
// by SemVer precedence; a version that is not SemVer is passed by.
func greatest(versions []string) (string, bool) {
	var best, bestPre string
	var bv, bpv semver
	for _, v := range versions {
		sv, err := parseVersion(v)
		switch {
		case err != nil:
		case sv.pre == nil && (best == "" || sv.compare(bv) > 0):
			best, bv = v, sv
		case sv.pre != nil && (bestPre == "" || sv.compare(bpv) > 0):
			bestPre, bpv = v, sv
		}
	}
	if best != "" {
		return best, true
	}
	return bestPre, bestPre != ""
}
