package format

import "errors"

var (
	// ErrInvalid means a manifest or an upload does not satisfy the manifest profile or the
	// format's own rules (docs/spec/manifest-profile.md §5). Frontends answer 400, the facade
	// MANIFEST_INVALID.
	ErrInvalid = errors.New("format: invalid")

	// ErrUnsupported means the plugin does not implement the operation, for example Build for a
	// format that is only published with oras push.
	ErrUnsupported = errors.New("format: unsupported")

	// ErrUnauthenticated means the request has no principal and the action needs one: Authorize
	// denied the anonymous principal. Frontends answer 401 with a Basic challenge.
	ErrUnauthenticated = errors.New("format: authentication required")

	// ErrForbidden means Authorize denied an authenticated principal. Frontends answer 403.
	ErrForbidden = errors.New("format: forbidden")
)
