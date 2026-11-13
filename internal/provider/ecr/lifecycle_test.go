package ecr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/smithy-go"
	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/config"
)

const (
	// policyAll keeps ten images of any tag: it takes versions, journal entries and keep-tags.
	policyAll = `{"rules":[{"rulePriority":1,"description":"keep ten","selection":{"tagStatus":"tagged","tagPatternList":["*"],` +
		`"countType":"imageCountMoreThan","countNumber":10},"action":{"type":"expire"}}]}`
	// policyUntagged expires untagged images after a day, and archives tags starting with v after 90.
	policyUntagged = `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"untagged","countType":"sinceImagePushed","countUnit":"days",` +
		`"countNumber":1},"action":{"type":"expire"}},{"rulePriority":2,"selection":{"tagStatus":"tagged","tagPrefixList":["v"],` +
		`"countType":"sinceImagePulled","countUnit":"days","countNumber":90},"action":{"type":"transition","targetStorageClass":"archive"}}]}`
)

// logLines decodes the JSON log lines in buf.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(buf.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		lines = append(lines, m)
	}
	return lines
}

// summary is each log line as "LEVEL msg-start", the message cut at its first colon.
func summary(lines []map[string]any) []string {
	var out []string
	for _, l := range lines {
		msg, _, _ := strings.Cut(l["msg"].(string), ":")
		out = append(out, fmt.Sprint(l["level"], " ", msg))
	}
	return out
}

// TestLifecycleRuleCatches: a rule of untagged images only spares what Lajstrom needs; any
// other can take versions, and the reserved tags it selects are named. One entry of a list that
// matches is enough, ECR's "all entries" notwithstanding: an image can hold several tags.
func TestLifecycleRuleCatches(t *testing.T) {
	all := []string{"keep-tags _keep.sha256.*", "npm journal _j*", "hashed version tags _h*", "referrers tag schema sha256-*"}
	tests := []struct {
		name      string
		selection string
		harms     bool
		kinds     []string
	}{
		{"untagged", `{"tagStatus":"untagged"}`, false, nil},
		{"any", `{"tagStatus":"any"}`, true, all},
		{"unknown status", `{"tagStatus":"ANY"}`, true, all},
		{"every tag", `{"tagStatus":"tagged","tagPatternList":["*"]}`, true, all},
		{"versions only", `{"tagStatus":"tagged","tagPrefixList":["v","release-"]}`, true, nil},
		{"underscore", `{"tagStatus":"tagged","tagPrefixList":["_"]}`, true, all[:3]},
		{"keep-tags", `{"tagStatus":"tagged","tagPatternList":["_keep.*"]}`, true, all[:1]},
		{"dots are literal", `{"tagStatus":"tagged","tagPatternList":["_kee..sha256.*"]}`, true, nil},
		{"referrers", `{"tagStatus":"tagged","tagPatternList":["sha256-*"]}`, true, all[3:]},
		{"one entry of several", `{"tagStatus":"tagged","tagPatternList":["prod*","_j*"]}`, true, all[1:2]},
	}
	for _, tt := range tests {
		var r lifecycleRule
		if err := json.Unmarshal([]byte(`{"rulePriority":1,"selection":`+tt.selection+`,"action":{"type":"expire"}}`), &r); err != nil {
			t.Fatal(err)
		}
		harms, kinds := r.catches()
		if harms != tt.harms || !slices.Equal(kinds, tt.kinds) {
			t.Errorf("%s: catches() = %v, %q; want %v, %q", tt.name, harms, kinds, tt.harms, tt.kinds)
		}
	}
}

// TestCheckLifecycle: the repositories below the instance prefix are read, each policy is
// logged once with the repositories that have it, a rule that can take what Lajstrom needs at
// warning and one of untagged images at info; a policy that does not parse is a warning.
func TestCheckLifecycle(t *testing.T) {
	f := newFakeECR(newClock())
	for _, n := range []string{"art/raw/a", "art/raw/b", "art/pypi/c", "art/oci/d", "art/npm/e", "other/x"} {
		f.repos[n] = ecr.CreateRepositoryInput{}
	}
	f.policies["art/raw/a"] = policyAll
	f.policies["art/raw/b"] = policyAll
	f.policies["art/pypi/c"] = policyUntagged
	f.policies["art/npm/e"] = `{"rules":[`
	f.policies["other/x"] = policyAll
	var buf bytes.Buffer
	p := fakeProvider(t, f, nil)
	p.log = slog.New(slog.NewJSONHandler(&buf, nil))
	p.checkLifecycle(t.Context())

	if n := f.count("GetLifecyclePolicy"); n != 5 {
		t.Errorf("GetLifecyclePolicy called %d times, want 5: the repositories below the prefix", n)
	}
	lines := logLines(t, &buf)
	want := []string{ // in the order of the names: npm/e, pypi/c, raw/a and raw/b
		"WARN an ECR lifecycle policy below the prefix could not be read",
		"INFO an ECR lifecycle rule below the prefix takes untagged images only",
		"WARN an ECR lifecycle rule below the prefix can expire or archive what Lajstrom needs",
		"WARN an ECR lifecycle rule below the prefix can expire or archive what Lajstrom needs",
		"INFO ECR lifecycle policies checked",
	}
	if diff := cmp.Diff(want, summary(lines)); diff != "" {
		t.Fatalf("log lines (-want +got):\n%s\n%s", diff, buf.String())
	}
	kinds := []any{}
	for _, r := range reservedTags {
		kinds = append(kinds, r.kind)
	}
	wants := map[int]map[string]any{
		3: {"repositories": []any{"raw/a", "raw/b"}, "rulePriority": 1.0, "description": "keep ten", "tagStatus": "tagged",
			"tagPatternList": []any{"*"}, "countType": "imageCountMoreThan", "countNumber": 10.0, "action": "expire", "reservedTags": kinds},
		2: {"repositories": []any{"pypi/c"}, "rulePriority": 2.0, "tagPrefixList": []any{"v"}, "countUnit": "days",
			"action": "transition", "reservedTags": nil},
		4: {"repositories": 5.0, "sampled": 5.0, "withPolicy": 4.0},
	}
	for i, want := range wants {
		got := map[string]any{}
		for k := range want {
			got[k] = lines[i][k]
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("log line %d (-want +got):\n%s", i, diff)
		}
	}
}

// TestCheckLifecycleSkipped: without ecr:GetLifecyclePolicy the check ends at the first refusal
// with one line that names the action; without a catalog, or a listing, it is skipped with one
// line; with no repository below the prefix it says nothing above debug.
func TestCheckLifecycleSkipped(t *testing.T) {
	tests := []struct {
		name     string
		repos    int
		edit     func(*config.Registry)
		listErr  error
		getErr   error
		want     string // the one line
		attempts int    // of GetLifecyclePolicy
	}{
		{"denied", 3, nil, nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform: ecr:GetLifecyclePolicy"},
			"INFO ECR lifecycle policies not checked", 1},
		{"throttled", 3, nil, nil, &smithy.GenericAPIError{Code: "ThrottlingException"}, "WARN ECR lifecycle policies not checked", 1},
		{"catalog false", 3, func(c *config.Registry) { c.Capabilities.Catalog = aws.Bool(false) }, nil, nil,
			"INFO ECR lifecycle policies not checked", 0},
		{"listing denied", 3, nil, &smithy.GenericAPIError{Code: "AccessDeniedException"}, nil, "WARN ECR lifecycle policies not checked", 0},
		{"no repository", 0, nil, nil, nil, "", 0},
	}
	for _, tt := range tests {
		f := newFakeECR(newClock())
		for i := range tt.repos {
			f.repos[fmt.Sprintf("art/raw/r%d", i)] = ecr.CreateRepositoryInput{}
		}
		f.listErr, f.policyErr = tt.listErr, tt.getErr
		var buf bytes.Buffer
		p := fakeProvider(t, f, tt.edit)
		p.log = slog.New(slog.NewJSONHandler(&buf, nil))
		p.checkLifecycle(t.Context())
		got := summary(logLines(t, &buf))
		if tt.want == "" && len(got) > 0 || tt.want != "" && !slices.Equal(got, []string{tt.want}) {
			t.Errorf("%s: log lines %q, want %q", tt.name, got, tt.want)
		}
		if n := f.count("GetLifecyclePolicy"); n != tt.attempts {
			t.Errorf("%s: GetLifecyclePolicy called %d times, want %d", tt.name, n, tt.attempts)
		}
		if tt.name == "denied" && !strings.Contains(buf.String(), `"action":"ecr:GetLifecyclePolicy"`) {
			t.Errorf("the refusal does not name the IAM action: %s", buf.String())
		}
	}
}

// TestCheckLifecycleSample: of many repositories, lifecycleSample are read, spread over the
// names in order.
func TestCheckLifecycleSample(t *testing.T) {
	f := newFakeECR(newClock())
	f.pageSize = 30
	var names []string
	for i := range 100 {
		n := fmt.Sprintf("raw/r%03d", i)
		names = append(names, n)
		f.repos["art/"+n] = ecr.CreateRepositoryInput{}
	}
	p := fakeProvider(t, f, nil)
	p.checkLifecycle(t.Context())
	if n := f.count("GetLifecyclePolicy"); n != lifecycleSample {
		t.Errorf("GetLifecyclePolicy called %d times, want %d", n, lifecycleSample)
	}
	got := spread(names, lifecycleSample)
	if len(got) != lifecycleSample || got[0] != "raw/r000" || got[1] != "raw/r005" || got[lifecycleSample-1] != "raw/r095" {
		t.Errorf("spread = %q", got)
	}
	if few := names[:3]; !slices.Equal(spread(few, lifecycleSample), few) {
		t.Errorf("spread of %d names is not all of them", len(few))
	}
}

// TestCapabilitiesChecksLifecycleOnce: the first answer of Capabilities starts the check, in the
// background; later answers do not start it again.
func TestCapabilitiesChecksLifecycleOnce(t *testing.T) {
	v2 := httptest.NewServer(http.HandlerFunc(ecrV2))
	t.Cleanup(v2.Close)
	f := newFakeECR(newClock())
	f.repos["art/raw/a"] = ecr.CreateRepositoryInput{}
	f.policies["art/raw/a"] = policyAll
	var buf bytes.Buffer
	p := fakeProvider(t, f, func(c *config.Registry) { c.URL = v2.URL })
	p.log = slog.New(slog.NewJSONHandler(&buf, nil))
	for range 3 {
		if _, err := p.Capabilities(t.Context()); err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
	}
	<-p.lifecycleDone
	if n, m := f.count("DescribeRepositories"), f.count("GetLifecyclePolicy"); n != 1 || m != 1 {
		t.Errorf("DescribeRepositories %d, GetLifecyclePolicy %d calls; want one check", n, m)
	}
	if !strings.Contains(buf.String(), "can expire or archive what Lajstrom needs") {
		t.Errorf("no warning:\n%s", buf.String())
	}
}
