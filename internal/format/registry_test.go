package format

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
)

// stub is the smallest Plugin: a name and nothing else.
type stub string

func (s stub) Name() string                    { return string(s) }
func (s stub) ArtifactType() string            { return "application/vnd.lajstrom." + string(s) + ".v1" }
func (stub) EscapeName(string) (string, error) { return "", ErrUnsupported }
func (stub) Routes(Router, Deps)               {}

func (stub) Validate(context.Context, index.Repo, ocispec.Manifest, BlobFetcher) (*Entry, error) {
	return nil, ErrUnsupported
}

func (stub) Build(context.Context, index.Repo, Upload) (*Built, error) {
	return nil, ErrUnsupported
}

func (stub) RenderIndex(context.Context, index.Repo, IndexScope, index.Reader) ([]Document, error) {
	return nil, ErrUnsupported
}

// isolate gives the test an empty registry and restores the previous one afterwards.
func isolate(t *testing.T) {
	t.Helper()
	registry.mu.Lock()
	saved := registry.plugins
	registry.plugins = map[string]Plugin{}
	registry.mu.Unlock()
	t.Cleanup(func() {
		registry.mu.Lock()
		registry.plugins = saved
		registry.mu.Unlock()
	})
}

// register calls Register and returns the panic message, or "" when it did not panic.
func register(p Plugin) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	Register(p)
	return ""
}

func names(ps []Plugin) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name())
	}
	return out
}

func TestRegister(t *testing.T) {
	tests := []struct {
		name      string
		plugins   []Plugin
		wantPanic string // substring of the panic of the last Register; "" means none
		wantAll   []string
	}{
		{name: "none", wantAll: []string{}},
		{name: "one", plugins: []Plugin{stub("raw")}, wantAll: []string{"raw"}},
		{
			name:    "sorted by name",
			plugins: []Plugin{stub("pypi"), stub("apt"), stub("raw"), stub("maven")},
			wantAll: []string{"apt", "maven", "pypi", "raw"},
		},
		{name: "nil plugin", plugins: []Plugin{nil}, wantPanic: "nil plugin", wantAll: []string{}},
		{name: "empty name", plugins: []Plugin{stub("")}, wantPanic: "empty name", wantAll: []string{}},
		{
			name:      "duplicate name",
			plugins:   []Plugin{stub("raw"), stub("apt"), stub("raw")},
			wantPanic: "twice for plugin raw",
			wantAll:   []string{"apt", "raw"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			var got string
			for i, p := range tt.plugins {
				got = register(p)
				if got != "" && i != len(tt.plugins)-1 {
					t.Fatalf("Register(%v) panicked early: %s", p, got)
				}
			}
			switch {
			case tt.wantPanic == "" && got != "":
				t.Errorf("Register panicked: %s", got)
			case tt.wantPanic != "" && !strings.Contains(got, tt.wantPanic):
				t.Errorf("Register panic = %q, want it to contain %q", got, tt.wantPanic)
			}
			if all := names(All()); !slices.Equal(all, tt.wantAll) {
				t.Errorf("All() = %v, want %v", all, tt.wantAll)
			}
		})
	}
}

func TestLookup(t *testing.T) {
	isolate(t)
	Register(stub("apt"))
	Register(stub("raw"))

	tests := []struct {
		name   string
		lookup string
		wantOK bool
	}{
		{name: "registered", lookup: "raw", wantOK: true},
		{name: "other registered", lookup: "apt", wantOK: true},
		{name: "unknown", lookup: "maven"},
		{name: "empty", lookup: ""},
		{name: "case sensitive", lookup: "RAW"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := Lookup(tt.lookup)
			if ok != tt.wantOK {
				t.Fatalf("Lookup(%q) ok = %v, want %v", tt.lookup, ok, tt.wantOK)
			}
			if ok && p.Name() != tt.lookup {
				t.Errorf("Lookup(%q) returned plugin %q", tt.lookup, p.Name())
			}
		})
	}
}
