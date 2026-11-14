package profile

import (
	"encoding/json"
	"testing"
)

// TestPredicateTool is the table of manifest-profile.md §6.2: the tool, version and database of
// each predicate a scanner writes, and what is left empty.
func TestPredicateTool(t *testing.T) {
	for _, tt := range []struct {
		name, predicateType, predicate string
		tool, version, database        string
	}{
		{"trivy cosign-vuln", PredicateCosignVuln,
			`{"scanner":{"uri":"pkg:github/aquasecurity/trivy@0.60.0","version":"0.60.0","db":{"uri":"","version":"2026-11-14T00:00:00Z"}}}`,
			"trivy", "0.60.0", "2026-11-14T00:00:00Z"},
		{"cosign-vuln, the version in the purl, the db's uri", PredicateCosignVuln,
			`{"scanner":{"uri":"pkg:golang/github.com/anchore/grype@v0.80.0?arch=amd64","db":{"uri":"https://example.com/db/v5"}}}`,
			"grype", "v0.80.0", "https://example.com/db/v5"},
		{"cosign-vuln, an uri that is no purl", PredicateCosignVuln, `{"scanner":{"uri":"clair","version":"4"}}`, "clair", "4", ""},
		{"CycloneDX 1.5 tools", PredicateCycloneDX,
			`{"metadata":{"tools":{"components":[{"type":"application","name":"syft","version":"1.18.0"},{"name":"other"}]}}}`, "syft", "1.18.0", ""},
		{"CycloneDX 1.4 tools", PredicateCycloneDX + "/v1.4", `{"metadata":{"tools":[{"vendor":"aquasecurity","name":"trivy","version":"0.60.0"}]}}`,
			"trivy", "0.60.0", ""},
		{"CycloneDX without tools", PredicateCycloneDX, `{"metadata":{}}`, "", "", ""},
		{"SPDX", PredicateSPDX, `{"creationInfo":{"creators":["Organization: Acme","Tool: trivy-0.60.0"]}}`, "trivy", "0.60.0", ""},
		{"SPDX, a tool with dashes", PredicateSPDX + "/v2.3", `{"creationInfo":{"creators":["Tool: spdx-sbom-generator-0.0.15"]}}`,
			"spdx-sbom-generator", "0.0.15", ""},
		{"SPDX, no version", PredicateSPDX, `{"creationInfo":{"creators":["Tool: scancode"]}}`, "scancode", "", ""},
		{"provenance", "https://slsa.dev/provenance/v1", `{"builder":{"id":"x"}}`, "", "", ""},
		{"not JSON", PredicateCosignVuln, `{`, "", "", ""},
		{"no predicate", PredicateCycloneDX, ``, "", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tool, version, database := PredicateTool(tt.predicateType, json.RawMessage(tt.predicate))
			if tool != tt.tool || version != tt.version || database != tt.database {
				t.Errorf("PredicateTool = %q %q %q, want %q %q %q", tool, version, database, tt.tool, tt.version, tt.database)
			}
		})
	}
}
