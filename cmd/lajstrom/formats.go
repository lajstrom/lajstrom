package main

// The format plugins this binary serves. Each registers itself with format.Register when it is
// imported, serve mounts servedPlugins, and the repo service accepts only these formats. A new
// format is one more import here.
import (
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	_ "github.com/lajstrom/lajstrom/internal/format/deb"   // apt: .deb packages; Build and Validate, indexes, signing, pool and upload (#39, #40, #41, #42)
	_ "github.com/lajstrom/lajstrom/internal/format/maven" // maven: releases; Build and Validate (#52), deploy through sessions (#54), resolve (#55)
	"github.com/lajstrom/lajstrom/internal/format/npm"     // npm: Build and Validate (#304), publish (#305), packuments and tarballs (#306), the CLI (#307, #308)
	_ "github.com/lajstrom/lajstrom/internal/format/pypi"  // pypi: wheels and sdists; Build and Validate (#31); the simple index and twine come with #32, #33
	_ "github.com/lajstrom/lajstrom/internal/format/raw"   // raw: any file under a path (#13)
)

// externalURLPlugin is a plugin that renders absolute URLs, npm's dist.tarball, and takes
// server.externalURL for them.
type externalURLPlugin interface {
	WithExternalURL(u string) format.Plugin
}

// accountsPlugin is a plugin that logs users in, npm's: it takes the authenticator and the
// tokens service.
type accountsPlugin interface {
	WithAccounts(a npm.Accounts) format.Plugin
}

// accounts are the authenticator and the tokens service as one npm.Accounts.
type accounts struct {
	*auth.Authenticator
	*auth.Tokens
}

// lookupIn returns a lookup of plugins by name, as format.Lookup is of the registered ones: the
// core of proxy and group repos renders a hosted member's documents with the plugin serve
// mounts, which has server.externalURL, so that it caches what the member's own routes do.
func lookupIn(plugins []format.Plugin) func(name string) (format.Plugin, bool) {
	byName := make(map[string]format.Plugin, len(plugins))
	for _, p := range plugins {
		byName[p.Name()] = p
	}
	return func(name string) (format.Plugin, bool) {
		p, ok := byName[name]
		return p, ok
	}
}

// servedPlugins returns format.All(), each plugin that renders absolute URLs given externalURL
// and each that logs users in given a, for serve to mount. Ingest and reindex keep
// format.Lookup: Build and Validate render nothing.
func servedPlugins(externalURL string, a npm.Accounts) []format.Plugin {
	plugins := format.All()
	for i, p := range plugins {
		if u, ok := p.(externalURLPlugin); ok {
			p = u.WithExternalURL(externalURL)
		}
		if u, ok := p.(accountsPlugin); ok && a != nil {
			p = u.WithAccounts(a)
		}
		plugins[i] = p
	}
	return plugins
}
