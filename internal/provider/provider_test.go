package provider

import "oras.land/oras-go/v2/registry/remote"

// The generic provider relies on oras-go's remote.Repository satisfying Repository. The
// assertion lives in a test file to keep the remote package out of this package's imports.
var _ Repository = (*remote.Repository)(nil)
