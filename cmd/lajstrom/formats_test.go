package main

import (
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/npm"
)

// TestServedPlugins: serve mounts every registered plugin once, and npm with the external URL its
// packuments render dist.tarball under and the accounts of npm login; the registered one, which
// ingest uses, keeps neither.
func TestServedPlugins(t *testing.T) {
	a := accounts{&auth.Authenticator{}, &auth.Tokens{}}
	plugins := servedPlugins("https://registry.example", a)
	if len(plugins) != len(format.All()) {
		t.Fatalf("%d plugins, %d registered", len(plugins), len(format.All()))
	}
	for _, p := range plugins {
		if p.Name() != npm.Name {
			continue
		}
		if got, ok := p.(npm.Plugin); !ok || got.ExternalURL != "https://registry.example" || got.Accounts != a {
			t.Errorf("the npm plugin served: %#v", p)
		}
	}
	if registered, _ := format.Lookup(npm.Name); registered.(npm.Plugin).ExternalURL != "" || registered.(npm.Plugin).Accounts != nil {
		t.Errorf("the registered npm plugin: %#v", registered)
	}
}
