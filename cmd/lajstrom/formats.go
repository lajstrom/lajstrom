package main

// The format plugins this binary serves. Each registers itself with format.Register when it is
// imported, serve mounts servedPlugins, and the repo service accepts only these formats. A new
// format is one more import here.
import (
	"github.com/lajstrom/lajstrom/internal/format"
	_ "github.com/lajstrom/lajstrom/internal/format/deb"   // apt: .deb packages; Build and Validate, indexes, signing, pool and upload (#39, #40, #41, #42)
	_ "github.com/lajstrom/lajstrom/internal/format/maven" // maven: releases; Build and Validate (#52), deploy through sessions (#54), resolve (#55)
	_ "github.com/lajstrom/lajstrom/internal/format/npm"   // npm: Build and Validate (#304), publish (#305), packuments and tarballs (#306)
	_ "github.com/lajstrom/lajstrom/internal/format/pypi"  // pypi: wheels and sdists; Build and Validate (#31); the simple index and twine come with #32, #33
	_ "github.com/lajstrom/lajstrom/internal/format/raw"   // raw: any file under a path (#13)
)

// externalURLPlugin is a plugin that renders absolute URLs, npm's dist.tarball, and takes
// server.externalURL for them.
type externalURLPlugin interface {
	WithExternalURL(u string) format.Plugin
}

// servedPlugins returns format.All(), each plugin that renders absolute URLs given externalURL,
// for serve to mount. Ingest and reindex keep format.Lookup: Build and Validate render nothing.
func servedPlugins(externalURL string) []format.Plugin {
	plugins := format.All()
	for i, p := range plugins {
		if u, ok := p.(externalURLPlugin); ok {
			plugins[i] = u.WithExternalURL(externalURL)
		}
	}
	return plugins
}
