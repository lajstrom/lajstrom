package main

// The format plugins this binary serves. Each registers itself with format.Register when it is
// imported, serve mounts format.All(), and the repo service accepts only these formats. A new
// format is one more import here.
import (
	_ "github.com/lajstrom/lajstrom/internal/format/raw" // raw: any file under a path (#13)
)
