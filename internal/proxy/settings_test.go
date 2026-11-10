package proxy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/format"
)

func TestParseSettings(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Settings
		json string // JSON of want
	}{
		{
			name: "defaults",
			raw:  `{"name": "npmjs"}`,
			want: Settings{Upstream: "npmjs", MetadataTTL: 10 * time.Minute, NegativeTTL: 2 * time.Minute},
			json: `{"name":"npmjs","metadataTTL":"10m","negativeTTL":"2m","offline":false,"allow":[],"deny":[]}`,
		},
		{
			name: "every key",
			raw: `{"name": "npmjs", "metadataTTL": "1h30m", "negativeTTL": "0s", "offline": true,
				"allow": ["*"], "deny": ["@acme/*", "*-internal"]}`,
			want: Settings{Upstream: "npmjs", MetadataTTL: 90 * time.Minute, Offline: true, Allow: []string{"*"},
				Deny: []string{"@acme/*", "*-internal"}},
			json: `{"name":"npmjs","metadataTTL":"1h30m","negativeTTL":"0s","offline":true,"allow":["*"],"deny":["@acme/*","*-internal"]}`,
		},
		{
			name: "the bounds",
			raw:  `{"name": "u", "metadataTTL": "168h", "negativeTTL": "24h"}`,
			want: Settings{Upstream: "u", MetadataTTL: 168 * time.Hour, NegativeTTL: 24 * time.Hour},
			json: `{"name":"u","metadataTTL":"168h","negativeTTL":"24h","offline":false,"allow":[],"deny":[]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSettings(json.RawMessage(tt.raw))
			if err != nil {
				t.Fatal(err)
			}
			if d := cmp.Diff(tt.want, got); d != "" {
				t.Errorf("(-want +got)\n%s", d)
			}
			if j := string(got.JSON()); j != tt.json {
				t.Errorf("JSON = %s, want %s", j, tt.json)
			}
			again, err := ParseSettings(got.JSON())
			if err != nil || !cmp.Equal(again, got, cmp.Comparer(func(a, b []string) bool { return len(a) == len(b) && strings.Join(a, " ") == strings.Join(b, " ") })) {
				t.Errorf("ParseSettings(JSON()) = %+v, %v; want %+v", again, err, got)
			}
		})
	}
}

func TestParseSettingsErrors(t *testing.T) {
	long := `"` + strings.Repeat("a", MaxPatternLen+1) + `"`
	many := `[` + strings.TrimSuffix(strings.Repeat(`"a",`, MaxPatterns+1), ",") + `]`
	tests := []struct{ raw, want string }{
		{``, "upstream: must be a JSON object"},
		{`null`, "upstream: must be a JSON object"},
		{`{}`, "upstream.name: must be one of upstreams[].name"},
		{`{"name": ""}`, "upstream.name: must be one of upstreams[].name"},
		{`{"name": "u"} {}`, "upstream: must be one JSON object"},
		{`{"name": "u", "url": "https://registry.npmjs.org/"}`, `upstream: "url" is not a key`},
		{`{"name": 7}`, "upstream.name: must not be a JSON number"},
		{`{"name": "u", "metadataTTL": "1w"}`, `upstream.metadataTTL: "1w" is not a duration from 0s to 168h`},
		{`{"name": "u", "metadataTTL": "169h"}`, `upstream.metadataTTL: "169h" is not a duration`},
		{`{"name": "u", "negativeTTL": "-1s"}`, `upstream.negativeTTL: "-1s" is not a duration from 0s to 24h`},
		{`{"name": "u", "offline": "yes"}`, "upstream.offline: must not be a JSON string"},
		{`{"name": "u", "allow": [""]}`, "upstream.allow[0]: must not be empty"},
		{`{"name": "u", "deny": ["ok", "has space"]}`, `upstream.deny[1]: "has space" has a space or a control character`},
		{`{"name": "u", "deny": ["tab\there"]}`, `upstream.deny[0]: "tab\there" has a space or a control character`},
		{`{"name": "u", "deny": [` + long + `]}`, "upstream.deny[0]: is longer than 256 bytes"},
		{`{"name": "u", "allow": ` + many + `}`, "upstream.allow: has 257 patterns, at most 256"},
	}
	for _, tt := range tests {
		_, err := ParseSettings(json.RawMessage(tt.raw))
		var se *SettingsError
		switch {
		case err == nil:
			t.Errorf("ParseSettings(%.40s): no error, want %q", tt.raw, tt.want)
		case !errors.As(err, &se) || !errors.Is(err, format.ErrInvalid):
			t.Errorf("ParseSettings(%.40s) = %v, want a *SettingsError wrapping format.ErrInvalid", tt.raw, err)
		case !strings.HasPrefix(err.Error(), tt.want):
			t.Errorf("ParseSettings(%.40s) = %q, want it to start with %q", tt.raw, err, tt.want)
		}
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, key string
		want         bool
	}{
		{"left-pad", "left-pad", true},
		{"left-pad", "Left-Pad", true},
		{"left-pad", "left-pads", false},
		{"*", "", true},
		{"*", "@acme/billing", true},
		{"@acme/*", "@acme/billing", true},
		{"@ACME/*", "@acme/billing", true},
		{"@acme/*", "@acme", false},
		{"@acme/*", "@acme-evil/billing", false},
		{"*-internal", "billing-internal", true},
		{"*-internal", "billing-internal-not", false},
		{"com.acme:*", "com.acme:lib-core", true},
		{"com.acme:*", "com.acmex:lib", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "aXbYbZc", true},
		{"a*b*c", "acb", false},
		{"a*a", "a", false},
		{"a**a", "aa", true},
		{"*ab*ab", "xabab", true},
		{"*ab*ab", "xab", false},
	}
	for _, tt := range tests {
		if got := Match(tt.pattern, tt.key); got != tt.want {
			t.Errorf("Match(%q, %q) = %t, want %t", tt.pattern, tt.key, got, tt.want)
		}
	}
}

func TestRefuses(t *testing.T) {
	tests := []struct {
		allow, deny []string
		key, why    string
	}{
		{nil, nil, "left-pad", ""},
		{nil, []string{"@acme/*"}, "@acme/billing", "deny: @acme/*"},
		{nil, []string{"@acme/*"}, "left-pad", ""},
		{[]string{"left-*", "@types/*"}, nil, "@types/node", ""},
		{[]string{"left-*", "@types/*"}, nil, "react", "allow"},
		{[]string{"*"}, []string{"@acme/*", "*-internal"}, "billing-internal", "deny: *-internal"}, // deny wins
	}
	for _, tt := range tests {
		s := Settings{Allow: tt.allow, Deny: tt.deny}
		why, refused := s.Refuses(tt.key)
		if why != tt.why || refused != (tt.why != "") {
			t.Errorf("allow %q, deny %q: Refuses(%q) = %q, %t; want %q", tt.allow, tt.deny, tt.key, why, refused, tt.why)
		}
	}
}
