package main

// The format plugins this binary serves. Each registers itself with format.Register when it is
// imported, serve mounts format.All(), and the repo service accepts only these formats. A new
// format is one more import here.
import (
	_ "github.com/lajstrom/lajstrom/internal/format/deb"   // apt: .deb packages; Build and Validate, indexes, signing, pool and upload (#39, #40, #41, #42)
	_ "github.com/lajstrom/lajstrom/internal/format/maven" // maven: releases; Build and Validate (#52), deploy through sessions (#54), resolve (#55)
	_ "github.com/lajstrom/lajstrom/internal/format/npm"   // npm: Build and Validate, oras push through /v2 (#304); publish and install come with #305, #306
	_ "github.com/lajstrom/lajstrom/internal/format/pypi"  // pypi: wheels and sdists; Build and Validate (#31); the simple index and twine come with #32, #33
	_ "github.com/lajstrom/lajstrom/internal/format/raw"   // raw: any file under a path (#13)
)
