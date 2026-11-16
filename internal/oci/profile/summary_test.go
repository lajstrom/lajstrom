package profile

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSummary: the summary is the first line of the type's field, trimmed and cut to 256
// characters, and "" when the type has no field or the metadata does not have it as a string
// (docs/spec/manifest-profile.md §2, The summary).
func TestSummary(t *testing.T) {
	long := strings.Repeat("é", 300)
	tests := []struct {
		name, typ, metadata, want string
	}{
		{"pypi", "pypi", `{"name": "requests", "summary": "Python HTTP for Humans."}`, "Python HTTP for Humans."},
		{"deb, its synopsis", "deb", `{"control": {"Package": "hello", "Description": "example package\n This is the long part.\n ."}}`, "example package"},
		{"npm", "npm", `{"name": "@acme/widgets", "manifest": {"description": "  Widgets for Acme\r\n"}}`, "Widgets for Acme"},
		{"rpm", "rpm", `{"name": "bash", "summary": "\tThe GNU Bourne Again shell "}`, "The GNU Bourne Again shell"},
		{"cut to 256 characters", "pypi", `{"summary": "` + long + `"}`, long[:2*maxSummary]},
		{"maven has none", "maven", `{"groupId": "com.example", "pom": {"description": "x"}}`, ""},
		{"raw has none", "raw", `{"path": "a/b", "contentType": "text/plain"}`, ""},
		{"no metadata", "pypi", ``, ""},
		{"not JSON", "pypi", `{"summary": `, ""},
		{"absent", "pypi", `{"name": "requests"}`, ""},
		{"null", "pypi", `{"summary": null}`, ""},
		{"not a string", "pypi", `{"summary": 7}`, ""},
		{"nothing left", "npm", `{"manifest": {"description": " \t\n second line"}}`, ""},
		{"deb control not an object", "deb", `{"control": "Description: x"}`, ""},
		{"a source package's .dsc", "deb", `{"control": {"Source": "hello", "Format": "3.0 (quilt)"}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var md json.RawMessage
			if tt.metadata != "" {
				md = json.RawMessage(tt.metadata)
			}
			if got := Summary(ArtifactType(tt.typ), md); got != tt.want {
				t.Errorf("Summary = %q, want %q", got, tt.want)
			}
		})
	}
	if got := Summary("application/vnd.example.v1", json.RawMessage(`{"summary": "x"}`)); got != "" {
		t.Errorf("Summary of a foreign artifact type = %q, want none", got)
	}
}
