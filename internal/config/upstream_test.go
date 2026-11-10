package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestLoadUpstreams: upstreams[] with the defaults of the keys they leave out, and proxy repos
// that name them (docs/spec/proxy-and-group.md §2.1).
func TestLoadUpstreams(t *testing.T) {
	file := minimal + `
registries:
  - {name: local, url: "http://zot:5000"}
upstreams:
  - {name: npmjs, format: npm, url: "https://registry.npmjs.org/"}
  - name: internal
    format: npm
    url: http://npm.internal:4873/registry/
    auth: {username: lajstrom, passwordFile: /run/secrets/npm-password}
    timeout: 5s
    retries: -1
    maxMetadataSize: 1048576
  - {name: token, format: npm, url: "https://npm.example", auth: {tokenFile: /run/secrets/npm-token}}
repos:
  - {name: internal, format: npm, registry: local}
  - {name: npmjs, format: npm, registry: local, kind: proxy, upstream: {name: npmjs, deny: ["@acme/*"]}}
`
	c, err := Load(writeConfig(t, file), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []Upstream{
		{Name: "npmjs", Format: "npm", URL: "https://registry.npmjs.org/", Timeout: 30 * time.Second, Retries: 2, MaxMetadataSize: 64 << 20},
		{Name: "internal", Format: "npm", URL: "http://npm.internal:4873/registry/",
			Auth:    UpstreamAuth{Username: "lajstrom", PasswordFile: "/run/secrets/npm-password"}, //nolint:gosec // file names, not credentials
			Timeout: 5 * time.Second, Retries: -1, MaxMetadataSize: 1 << 20},
		{Name: "token", Format: "npm", URL: "https://npm.example", Auth: UpstreamAuth{TokenFile: "/run/secrets/npm-token"}, //nolint:gosec // a file name, not a credential
			Timeout: 30 * time.Second, Retries: 2, MaxMetadataSize: 64 << 20},
	}
	if !reflect.DeepEqual(c.Upstreams, want) {
		t.Errorf("upstreams =\n%+v\nwant\n%+v", c.Upstreams, want)
	}
	for i, insecure := range []bool{false, true, false} {
		if got := c.Upstreams[i].Insecure(); got != insecure {
			t.Errorf("upstreams[%d].Insecure() = %t, want %t", i, got, insecure)
		}
	}
	if r := c.Repos[1]; r.Kind != KindProxy || r.Upstream["name"] != "npmjs" {
		t.Errorf("repos[1] = %+v, want a proxy of npmjs", r)
	}
}

func TestLoadUpstreamErrors(t *testing.T) {
	const registries = "registries:\n  - {name: local, url: \"http://zot:5000\"}\n"
	tests := []struct {
		name string
		file string
		want []string // substrings of the error, one problem each
	}{
		{ //nolint:gosec // a fake password, which the error must not show
			name: "upstream keys",
			file: `upstreams:
  - {name: "", format: "", url: "registry.npmjs.org", timeout: -1s, maxMetadataSize: -1}
  - {name: npmjs, format: npm, url: "https://u:secret@registry.npmjs.org/"}
  - {name: npmjs, format: npm, url: "https://registry.npmjs.org/?x=1"}
  - {name: frag, format: npm, url: "https://registry.npmjs.org/#top"}
`,
			want: []string{
				"upstreams[0].name: must not be empty", "upstreams[0].format: must not be empty",
				"upstreams[0].url: must be an absolute http(s) URL", "upstreams[0].timeout: must not be negative",
				"upstreams[0].maxMetadataSize: must not be negative",
				"upstreams[1].url: must not contain credentials", `upstreams[2].name: "npmjs" is already used by upstreams[1]`,
				"upstreams[2].url: must not have a query or a fragment", "upstreams[3].url: must not have a query or a fragment",
			},
		},
		{
			name: "credentials",
			file: `upstreams:
  - {name: a, format: npm, url: "https://a.example", auth: {username: u}}
  - {name: b, format: npm, url: "https://b.example", auth: {passwordFile: /p}}
  - {name: c, format: npm, url: "https://c.example", auth: {username: u, passwordFile: /p, tokenFile: /t}}
`,
			want: []string{
				"upstreams[0].auth: username and passwordFile must be set together",
				"upstreams[1].auth: username and passwordFile must be set together",
				"upstreams[2].auth: tokenFile is a bearer token: not with username and passwordFile",
			},
		},
		{
			name: "proxy repos",
			file: registries + `upstreams:
  - {name: npmjs, format: npm, url: "https://registry.npmjs.org/"}
repos:
  - {name: a, format: npm, registry: local, kind: mirror}
  - {name: b, format: npm, registry: local, kind: proxy}
  - {name: c, format: npm, registry: local, kind: proxy, upstream: {deny: ["@acme/*"]}}
  - {name: d, format: npm, registry: local, kind: proxy, upstream: {name: pypi}}
  - {name: e, format: pypi, registry: local, kind: proxy, upstream: {name: npmjs}}
  - {name: f, format: npm, registry: local, upstream: {name: npmjs}}
  - {name: g, format: npm, registry: local, kind: hosted, upstream: {name: npmjs}}
`,
			want: []string{
				`repos[0].kind: "mirror" is not hosted or proxy`,
				"repos[1].upstream: a proxy repo must name one of upstreams[].name",
				"repos[2].upstream.name: must be one of upstreams[].name",
				`repos[3].upstream.name: "pypi" is not a configured upstream`,
				`repos[4].upstream.name: "npmjs" serves npm, not pypi`,
				"repos[5].upstream: only a proxy repo has one, not a hosted repo",
				"repos[6].upstream: only a proxy repo has one, not a hosted repo",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, minimal+tt.file), nil)
			if err == nil {
				t.Fatal("Load: no error")
			}
			msg := err.Error()
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error does not contain %q:\n%s", w, msg)
				}
			}
			if lines := strings.Count(msg, "\n"); lines != len(tt.want) {
				t.Errorf("error has %d problems, want %d:\n%s", lines, len(tt.want), msg)
			}
			if strings.Contains(msg, "secret") {
				t.Errorf("error contains a credential:\n%s", msg)
			}
		})
	}
}
