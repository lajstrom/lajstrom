package provider

import (
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
)

// orasRepository is the part of Repository that oras-go's remote.Repository implements; the
// generic provider wraps it and adds the BlobUploader. The assertion lives in a test file to
// keep the remote package out of this package's imports.
type orasRepository interface {
	content.Storage
	content.Deleter
	content.TagResolver
	registry.ReferencePusher
	registry.TagLister
	registry.ReferrerLister
}

var _ orasRepository = (*remote.Repository)(nil)
