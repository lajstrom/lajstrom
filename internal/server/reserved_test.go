package server

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
)

func TestIsReserved(t *testing.T) {
	tests := map[string]bool{
		"v2": true, "api": true, "ui": true, "-": true, ".well-known": true,
		"raw": false, "pypi": false, "": false, "API": false, "v2x": false,
	}
	for seg, want := range tests {
		if got := IsReserved(seg); got != want {
			t.Errorf("IsReserved(%q) = %v, want %v", seg, got, want)
		}
	}
}

func TestNewChecksPluginNames(t *testing.T) {
	tests := []struct {
		name    string
		plugins []format.Plugin
		want    []string // substrings of the error; nil means New succeeds
	}{
		{name: "none"},
		{name: "valid", plugins: []format.Plugin{stubPlugin("raw"), stubPlugin("pypi")}},
		{name: "api", plugins: []format.Plugin{stubPlugin("raw"), stubPlugin("api")}, want: []string{`plugin "api"`, "reserved"}},
		{name: "v2", plugins: []format.Plugin{stubPlugin("v2")}, want: []string{`plugin "v2"`, "reserved"}},
		{name: "dash", plugins: []format.Plugin{stubPlugin("-")}, want: []string{`plugin "-"`, "reserved"}},
		{name: "well-known", plugins: []format.Plugin{stubPlugin(".well-known")}, want: []string{`plugin ".well-known"`, "reserved"}},
		{name: "ui", plugins: []format.Plugin{stubPlugin("ui")}, want: []string{`plugin "ui"`, "reserved"}},
		{name: "slash", plugins: []format.Plugin{stubPlugin("apt/deb")}, want: []string{`plugin "apt/deb"`, "one URL segment"}},
		{name: "empty", plugins: []format.Plugin{stubPlugin("")}, want: []string{"empty name"}},
		{name: "nil", plugins: []format.Plugin{nil}, want: []string{"plugin 0 is nil"}},
		{name: "twice", plugins: []format.Plugin{stubPlugin("raw"), stubPlugin("raw")}, want: []string{`plugin "raw" is registered twice`}},
		{
			name:    "every problem is reported",
			plugins: []format.Plugin{stubPlugin("api"), stubPlugin("a/b")},
			want:    []string{`plugin "api"`, `plugin "a/b"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(Options{Listen: ":0", Plugins: tt.plugins, Log: slog.New(slog.DiscardHandler)})
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

func TestNewLogsFormats(t *testing.T) {
	_, buf := newTestServer(t, Options{Plugins: []format.Plugin{stubPlugin("raw")}})
	l := findLog(t, buf, "format registered")
	if l["format"] != "raw" || l["artifactType"] != "application/vnd.lajstrom.raw.v1" {
		t.Errorf("log line = %v", l)
	}
}
