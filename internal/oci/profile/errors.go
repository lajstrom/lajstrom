package profile

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalid is the root of every error of this package: a manifest, an annotation set or an
	// escaped string does not satisfy docs/spec/manifest-profile.md. Every other sentinel wraps
	// it, so errors.Is(err, ErrInvalid) is the one test ingest needs to answer 400.
	ErrInvalid = errors.New("profile: invalid")

	// ErrInvalidEscape means Unescape got a string that Escape cannot produce.
	ErrInvalidEscape = fmt.Errorf("%w escape", ErrInvalid)

	// ErrMediaType means the manifest is not an OCI image manifest (mediaType or schemaVersion).
	ErrMediaType = fmt.Errorf("%w manifest mediaType", ErrInvalid)

	// ErrArtifactType means artifactType is not application/vnd.lajstrom.<type>.v1 of a known
	// type, or Build was given a lajstrom.format that maps to no type.
	ErrArtifactType = fmt.Errorf("%w artifactType", ErrInvalid)

	// ErrConfig means config is not the OCI empty descriptor (EmptyConfig).
	ErrConfig = fmt.Errorf("%w config", ErrInvalid)

	// ErrAnnotation means a required annotation is missing or empty, lajstrom.schema is not
	// SchemaVersion, or org.opencontainers.image.created is not an RFC 3339 time.
	ErrAnnotation = fmt.Errorf("%w annotation", ErrInvalid)

	// ErrFormatMismatch means lajstrom.format is not the protocol format of the artifact type,
	// for example artifactType deb with lajstrom.format pypi.
	ErrFormatMismatch = fmt.Errorf("%w lajstrom.format for the artifactType", ErrInvalid)

	// ErrNoLayers means the manifest has no file layer (a metadata layer alone is not enough).
	ErrNoLayers = fmt.Errorf("%w layers: no file layer", ErrInvalid)

	// ErrLayerMediaType means a layer media type is neither a file role of the type nor the
	// type's metadata media type.
	ErrLayerMediaType = fmt.Errorf("%w layer mediaType", ErrInvalid)

	// ErrLayerDescriptor means a layer descriptor is malformed: the digest is not
	// <algorithm>:<encoded> of a known algorithm, or the size is negative.
	ErrLayerDescriptor = fmt.Errorf("%w layer descriptor", ErrInvalid)

	// ErrLayerTitle means a layer has no org.opencontainers.image.title, or a file name with a
	// directory in a type other than raw.
	ErrLayerTitle = fmt.Errorf("%w layer title", ErrInvalid)

	// ErrMetadata means the metadata layer breaks a rule: more than one, not the last layer,
	// missing for a type that requires it, or not titled metadata.json.
	ErrMetadata = fmt.Errorf("%w metadata layer", ErrInvalid)
)
