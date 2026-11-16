package npm

import (
	"encoding/json"
	"testing"
)

// TestTrim: the versions a proxy's policy refuses leave versions, time and dist-tags; latest
// then names the greatest release left; a packument that loses nothing comes back as it was.
func TestTrim(t *testing.T) {
	kept := []byte(`{"name":"left-pad","dist-tags":{"latest":"1.3.1","next":"2.0.0-rc.1","old":"1.0.0"},` +
		`"versions":{"1.0.0":{"version":"1.0.0"},"1.3.0":{"version":"1.3.0"},"1.3.1":{"version":"1.3.1"},"2.0.0-rc.1":{"version":"2.0.0-rc.1"}},` +
		`"time":{"created":"2016-01-01T00:00:00Z","1.3.1":"2026-11-15T09:00:00Z","1.3.0":"2016-05-01T00:00:00Z"}}`)
	out, err := Plugin{}.Trim("left-pad", kindPackument, kept, func(v, _ string) bool { return v == "1.3.1" || v == "2.0.0-rc.1" })
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		DistTags map[string]string          `json:"dist-tags"`
		Versions map[string]json.RawMessage `json:"versions"`
		Time     map[string]string          `json:"time"`
		Name     string                     `json:"name"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Versions) != 2 || doc.Versions["1.3.1"] != nil || doc.Time["1.3.1"] != "" || doc.Time["created"] == "" || doc.Name != "left-pad" {
		t.Errorf("trimmed: %s", out)
	}
	if doc.DistTags["latest"] != "1.3.0" || doc.DistTags["old"] != "1.0.0" || doc.DistTags["next"] != "" {
		t.Errorf("dist-tags: %v, want latest moved to 1.3.0, next gone, old kept", doc.DistTags)
	}
	same, err := Plugin{}.Trim("left-pad", kindPackument, kept, func(string, string) bool { return false })
	if err != nil || string(same) != string(kept) {
		t.Errorf("a trim that drops nothing changed the packument: %v", err)
	}
}

// TestGreatest: a release before prereleases, a prerelease when there is none, nothing of no
// SemVer version.
func TestGreatest(t *testing.T) {
	if latest, ok := greatest([]string{"2.0.0-rc.1", "not-semver"}); !ok || latest != "2.0.0-rc.1" {
		t.Errorf("greatest of prereleases alone: %q %v", latest, ok)
	}
	if latest, ok := greatest([]string{"2.0.0-rc.1", "1.9.0", "1.10.0"}); !ok || latest != "1.10.0" {
		t.Errorf("greatest with releases: %q %v", latest, ok)
	}
	if _, ok := greatest([]string{"x"}); ok {
		t.Error("greatest of no SemVer version")
	}
}
