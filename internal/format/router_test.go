package format

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

func TestMountPattern(t *testing.T) {
	tests := []struct {
		pattern string
		want    string // the mounted pattern for plugin pypi
		wantErr string // substring of the error; "" means none
	}{
		{pattern: "GET /{repo}/simple/{project}/", want: "GET /pypi/{repo}/simple/{project}/"},
		{pattern: "POST /{repo}/", want: "POST /pypi/{repo}/"},
		{pattern: "GET /", want: "GET /pypi/"},
		{pattern: "GET /{$}", want: "GET /pypi/{$}"},
		{pattern: "/", want: "/pypi/"},
		{pattern: "/{repo}/files/{path...}", want: "/pypi/{repo}/files/{path...}"},
		{pattern: "GET\t/ping", want: "GET /pypi/ping"},
		{pattern: "GET   /ping", want: "GET /pypi/ping"},
		{pattern: "/a b", want: "/pypi/a b"}, // the part before the space has a "/": no method
		{pattern: "", wantErr: "empty pattern"},
		{pattern: "GET", wantErr: `pattern "GET" has no /path`},
		{pattern: "GET ", wantErr: `pattern "GET " has no /path`},
		{pattern: "ping", wantErr: "has no /path"},
		{pattern: "example.com/x", wantErr: `pattern "example.com/x" has a host`},
		{pattern: "GET example.com/{repo}/", wantErr: "has a host"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			got, err := mountPattern("pypi", tt.pattern)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("mountPattern(%q): %v", tt.pattern, err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("mountPattern(%q) = %q, %v; want an error containing %q", tt.pattern, got, err, tt.wantErr)
			case got != tt.want:
				t.Errorf("mountPattern(%q) = %q, want %q", tt.pattern, got, tt.want)
			}
		})
	}
}

// nopStore is an index.Store whose methods panic; NewRouter only checks that there is one.
type nopStore struct{ index.Store }

func TestRouterPanics(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	deps := Deps{Index: nopStore{}}
	tests := []struct {
		name string
		call func()
		want string // substring of the panic
	}{
		{name: "nil mux", call: func() { NewRouter(nil, "fake", deps) }, want: "nil mux"},
		{name: "nil index", call: func() { NewRouter(http.NewServeMux(), "fake", Deps{}) }, want: "nil Deps.Index"},
		{name: "reserved name", call: func() { NewRouter(http.NewServeMux(), "api", deps) }, want: `"api" is a reserved`},
		{name: "empty pattern", call: func() { NewRouter(http.NewServeMux(), "fake", deps).Handle("", ok) }, want: "empty pattern"},
		{name: "no path", call: func() { NewRouter(http.NewServeMux(), "fake", deps).Handle("GET", ok) }, want: "no /path"},
		{name: "host", call: func() { NewRouter(http.NewServeMux(), "fake", deps).HandleFunc("example.com/x", ok) }, want: "has a host"},
		{name: "nil handler", call: func() { NewRouter(http.NewServeMux(), "fake", deps).Handle("GET /x", nil) }, want: "nil handler"},
		{name: "nil handler func", call: func() { NewRouter(http.NewServeMux(), "fake", deps).HandleFunc("GET /{repo}/x", nil) }, want: "nil handler"},
		{
			name: "conflict is the ServeMux's",
			call: func() {
				r := NewRouter(http.NewServeMux(), "fake", deps)
				r.Handle("GET /{repo}/x", ok)
				r.Handle("GET /{name}/x", ok)
			},
			want: "conflicts with",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := func() (msg string) {
				defer func() { msg = fmt.Sprint(recover()) }()
				tt.call()
				return ""
			}()
			if !strings.Contains(got, tt.want) {
				t.Errorf("panic = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}
