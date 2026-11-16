package profile

import (
	"encoding/json"
	"strings"
)

// summaryFields is the path of the field of each type's metadata layer that holds its summary
// (docs/spec/manifest-profile.md §2, The summary). maven and raw have none.
var summaryFields = map[string][]string{
	"pypi": {"summary"},
	"deb":  {"control", "Description"},
	"npm":  {"manifest", "description"},
	"rpm":  {"summary"},
}

// maxSummary is the most characters a summary keeps.
const maxSummary = 256

// Summary returns the summary of a manifest of artifactType whose metadata layer is metadata:
// the first line of the type's field, without the spaces, tabs and carriage returns around it,
// at most maxSummary characters of it. It is "" when the type has no such field, metadata is
// nil or not JSON, or the field is absent, not a string or leaves nothing.
func Summary(artifactType string, metadata json.RawMessage) string {
	typ, _ := ParseArtifactType(artifactType)
	path := summaryFields[typ]
	if path == nil || len(metadata) == 0 {
		return ""
	}
	raw := metadata
	for _, key := range path {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return ""
		}
		if raw = object[key]; raw == nil {
			return ""
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	s, _, _ = strings.Cut(s, "\n")
	s = strings.Trim(s, " \t\r")
	if r := []rune(s); len(r) > maxSummary {
		s = string(r[:maxSummary])
	}
	return s
}
