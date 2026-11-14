package profile

import (
	"encoding/json"
	"strings"
)

// PredicateTool returns the tool, its version and the vulnerability database that the predicate of
// an in-toto statement of predicateType names (docs/spec/manifest-profile.md §6.2), for an
// attachment whose lajstrom.attachment.* annotations leave them out: cosign's vulnerability report
// (trivy --format cosign-vuln) has its scanner and database, a CycloneDX BOM its first tool, an
// SPDX document its first "Tool:" creator. What a predicate does not say, or one that does not
// parse, is "".
func PredicateTool(predicateType string, predicate json.RawMessage) (tool, version, database string) {
	if len(predicate) == 0 {
		return "", "", ""
	}
	switch {
	case predicateType == PredicateCosignVuln:
		return vulnTool(predicate)
	case isType(predicateType, PredicateCycloneDX):
		tool, version = cycloneDXTool(predicate)
		return tool, version, ""
	case isType(predicateType, PredicateSPDX):
		tool, version = spdxTool(predicate)
		return tool, version, ""
	}
	return "", "", ""
}

// vulnTool reads cosign's vulnerability predicate: scanner.uri, a purl whose name is the tool
// (pkg:github/aquasecurity/trivy@0.60.0), scanner.version, and scanner.db.
func vulnTool(predicate json.RawMessage) (tool, version, database string) {
	var p struct {
		Scanner struct {
			URI     string `json:"uri"`
			Version string `json:"version"`
			DB      struct {
				URI     string `json:"uri"`
				Version string `json:"version"`
			} `json:"db"`
		} `json:"scanner"`
	}
	if json.Unmarshal(predicate, &p) != nil {
		return "", "", ""
	}
	tool, purlVersion := purlName(p.Scanner.URI)
	version = p.Scanner.Version
	if version == "" {
		version = purlVersion
	}
	database = p.Scanner.DB.Version
	if database == "" {
		database = p.Scanner.DB.URI
	}
	return tool, version, database
}

// purlName returns the name and the version of the package URL uri, pkg:<type>/<namespace>/<name>@<version>;
// an uri that is no purl is the name itself.
func purlName(uri string) (name, version string) {
	rest, ok := strings.CutPrefix(uri, "pkg:")
	if !ok {
		return uri, ""
	}
	rest, _, _ = strings.Cut(rest, "?")
	rest, _, _ = strings.Cut(rest, "#")
	rest, version, _ = strings.Cut(rest, "@")
	if i := strings.LastIndex(rest, "/"); i >= 0 {
		rest = rest[i+1:]
	}
	return rest, version
}

// cycloneDXTool reads metadata.tools of a CycloneDX BOM: an object with components (1.5 and
// later) or a list (before), the first entry's name and version.
func cycloneDXTool(predicate json.RawMessage) (tool, version string) {
	var p struct {
		Metadata struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"metadata"`
	}
	if json.Unmarshal(predicate, &p) != nil || len(p.Metadata.Tools) == 0 {
		return "", ""
	}
	type entry struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	var current struct {
		Components []entry `json:"components"`
	}
	if json.Unmarshal(p.Metadata.Tools, &current) == nil && len(current.Components) > 0 {
		return current.Components[0].Name, current.Components[0].Version
	}
	var legacy []entry
	if json.Unmarshal(p.Metadata.Tools, &legacy) == nil && len(legacy) > 0 {
		return legacy[0].Name, legacy[0].Version
	}
	return "", ""
}

// spdxTool reads creationInfo.creators of an SPDX document: the first "Tool: <name>-<version>",
// split at its last "-".
func spdxTool(predicate json.RawMessage) (tool, version string) {
	var p struct {
		CreationInfo struct {
			Creators []string `json:"creators"`
		} `json:"creationInfo"`
	}
	if json.Unmarshal(predicate, &p) != nil {
		return "", ""
	}
	for _, c := range p.CreationInfo.Creators {
		t, ok := strings.CutPrefix(c, "Tool:")
		if !ok {
			continue
		}
		t = strings.TrimSpace(t)
		if i := strings.LastIndex(t, "-"); i > 0 {
			return t[:i], t[i+1:]
		}
		return t, ""
	}
	return "", ""
}
