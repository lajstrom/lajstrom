package provider

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
)

// stub is a Provider that only knows its name; the factory never calls the rest.
type stub struct {
	Provider
	name string
}

func (s stub) Name() string { return s.name }

// stubConstructor builds a stub, or fails for the registry named "broken".
func stubConstructor(cfg config.Registry, log *slog.Logger) (Provider, error) {
	if log == nil {
		return nil, errors.New("nil logger")
	}
	if cfg.Name == "broken" {
		return nil, errors.New("cannot build")
	}
	return stub{name: cfg.Name}, nil
}

// The tests register types in the package registry, which lives for the test binary: each
// test uses types of its own.

func TestRegisterPanics(t *testing.T) {
	Register("factory-panics", stubConstructor)
	tests := []struct {
		name string
		typ  string
		c    Constructor
		want string
	}{
		{name: "empty type", typ: "", c: stubConstructor, want: "empty type"},
		{name: "nil constructor", typ: "factory-nil", c: nil, want: "nil constructor"},
		{name: "duplicate", typ: "factory-panics", c: stubConstructor, want: "twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if s, ok := r.(string); !ok || !strings.Contains(s, tt.want) {
					t.Errorf("panic = %v, want one containing %q", r, tt.want)
				}
			}()
			Register(tt.typ, tt.c)
		})
	}
	if slices.Contains(Types(), "factory-nil") {
		t.Error("a refused type was registered")
	}
}

func TestNew(t *testing.T) {
	Register("factory-new-b", stubConstructor)
	Register("factory-new-a", stubConstructor)

	p, err := New(config.Registry{Name: "local", Type: "factory-new-a"}, nil)
	if err != nil || p.Name() != "local" {
		t.Fatalf("New = %v, %v; want the stub named local", p, err)
	}
	types := Types()
	if !slices.IsSorted(types) || !slices.Contains(types, "factory-new-a") || !slices.Contains(types, "factory-new-b") {
		t.Errorf("Types() = %q, want them sorted and with factory-new-a and factory-new-b", types)
	}

	_, err = New(config.Registry{Name: "local", Type: "harbor"}, nil)
	if err == nil {
		t.Fatal("New with an unknown type succeeded")
	}
	for _, want := range []string{`registry "local"`, `unknown type "harbor"`, "factory-new-a, factory-new-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	_, err = New(config.Registry{Name: "broken", Type: "factory-new-a"}, nil)
	if err == nil || err.Error() != `provider: registry "broken": cannot build` {
		t.Errorf("New error = %v, want the constructor's error with the registry name", err)
	}
}

func TestNewSet(t *testing.T) {
	Register("factory-set", stubConstructor)
	m, err := NewSet([]config.Registry{
		{Name: "local", Type: "factory-set"},
		{Name: "remote", Type: "factory-set"},
	}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"local", "remote"} {
		if p, ok := m.Provider(name); !ok || p.Name() != name {
			t.Errorf("Provider(%q) = %v, %v", name, p, ok)
		}
	}
	if _, ok := m.Provider("missing"); ok {
		t.Error(`Provider("missing") found one`)
	}

	empty, err := NewSet(nil, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("NewSet(nil) = %v, %v; want an empty map", empty, err)
	}

	_, err = NewSet([]config.Registry{
		{Name: "local", Type: "factory-set"},
		{Name: "local", Type: "factory-set"},
		{Name: "broken", Type: "factory-set"},
		{Name: "x", Type: "nope"},
	}, nil)
	if err == nil {
		t.Fatal("NewSet succeeded with broken registries")
	}
	for _, want := range []string{`"local" is configured twice`, `"broken": cannot build`, `unknown type "nope"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not report %q:\n%s", want, err)
		}
	}
}
