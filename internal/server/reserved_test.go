package server

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
)

func TestNewChecksPlugins(t *testing.T) {
	tests := []struct {
		name    string
		plugins []format.Plugin
		noIndex bool     // leave Deps.Index nil
		want    []string // substrings of the error; nil means New succeeds
	}{
		{name: "none"},
		{name: "none without an index", noIndex: true},
		{name: "valid", plugins: []format.Plugin{stubPlugin("raw"), stubPlugin("pypi")}},
		{name: "api", plugins: []format.Plugin{stubPlugin("raw"), stubPlugin("api")}, want: []string{"format plugin 1", `"api" is a reserved URL segment`}},
		{name: "v2", plugins: []format.Plugin{stubPlugin("v2")}, want: []string{`"v2" is a reserved`}},
		{name: "dash", plugins: []format.Plugin{stubPlugin("-")}, want: []string{`"-" is a reserved`}},
		{name: "well-known", plugins: []format.Plugin{stubPlugin(".well-known")}, want: []string{`".well-known" is a reserved`}},
		{name: "ui", plugins: []format.Plugin{stubPlugin("ui")}, want: []string{`"ui" is a reserved`}},
		{name: "slash", plugins: []format.Plugin{stubPlugin("apt/deb")}, want: []string{`"apt/deb": want lowercase`}},
		{name: "upper case", plugins: []format.Plugin{stubPlugin("Raw")}, want: []string{`"Raw": want lowercase`}},
		{name: "empty", plugins: []format.Plugin{stubPlugin("")}, want: []string{"format plugin 0 (server.stubPlugin): plugin name is empty"}},
		{name: "nil", plugins: []format.Plugin{nil}, want: []string{"plugin 0 is nil"}},
		{name: "twice", plugins: []format.Plugin{stubPlugin("raw"), stubPlugin("raw")}, want: []string{`plugin "raw" is registered twice`}},
		{
			name:    "every problem is reported",
			plugins: []format.Plugin{stubPlugin("api"), stubPlugin("a/b")},
			want:    []string{`"api"`, `"a/b"`},
		},
		{name: "plugins without an index", plugins: []format.Plugin{stubPlugin("raw")}, noIndex: true, want: []string{"format plugins need Deps.Index"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := Options{Listen: ":0", Plugins: tt.plugins, Log: slog.New(slog.DiscardHandler)}
			if !tt.noIndex {
				o.Deps.Index = &formattest.Index{}
			}
			_, err := New(o)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("New succeeded, want an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}
