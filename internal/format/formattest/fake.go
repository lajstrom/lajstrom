// Package formattest provides test doubles for server and plugin tests: Fake, a format plugin
// with a few routes, and Index, a partial index.Store. Non-test code does not import it; it is
// a normal package so that the tests of other packages can.
package formattest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Fake is a format plugin for tests. Its routes, relative to /<name>/:
//
//	GET /ping               200 "pong\n"
//	GET /{repo}/ping        200 "pong <format>/<repo>\n", from format.RepoFrom
//	GET /{repo}/fail/{kind} format.WriteError with the error kind names: invalid,
//	                        unauthenticated, forbidden, notfound, conflict, unsupported,
//	                        unauthorized (provider.ErrUnauthorized) or other (a plain error)
//
// and whatever Mount adds. Validate and Build call ValidateFunc and BuildFunc, and return
// format.ErrUnsupported when those are nil.
type Fake struct {
	// FormatName is the plugin name; empty means "fake".
	FormatName string
	// Mount, when set, is called at the end of Routes to mount more handlers.
	Mount func(r format.Router, deps format.Deps)
	// Render implements RenderIndex; nil makes RenderIndex return format.ErrUnsupported.
	Render func(ctx context.Context, repo index.Repo, scope format.IndexScope) ([]format.Document, error)
	// BuildFunc implements Build; nil makes Build return format.ErrUnsupported.
	BuildFunc func(ctx context.Context, repo index.Repo, up format.Upload) (*format.Built, error)
	// ValidateFunc implements Validate; nil makes Validate return format.ErrUnsupported.
	ValidateFunc func(ctx context.Context, repo index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error)
}

// Name returns FormatName, or "fake" when it is empty.
func (f Fake) Name() string {
	if f.FormatName == "" {
		return "fake"
	}
	return f.FormatName
}

// ArtifactType returns application/vnd.lajstrom.<name>.v1.
func (f Fake) ArtifactType() string { return "application/vnd.lajstrom." + f.Name() + ".v1" }

// EscapeName returns name unchanged; an empty name gives an error wrapping format.ErrInvalid.
func (Fake) EscapeName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty package name", format.ErrInvalid)
	}
	return name, nil
}

// Validate calls ValidateFunc, or returns format.ErrUnsupported when it is nil.
func (f Fake) Validate(ctx context.Context, repo index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
	if f.ValidateFunc == nil {
		return nil, format.ErrUnsupported
	}
	return f.ValidateFunc(ctx, repo, m, fetch)
}

// Build calls BuildFunc, or returns format.ErrUnsupported when it is nil.
func (f Fake) Build(ctx context.Context, repo index.Repo, up format.Upload) (*format.Built, error) {
	if f.BuildFunc == nil {
		return nil, format.ErrUnsupported
	}
	return f.BuildFunc(ctx, repo, up)
}

// RenderIndex calls Render, or returns format.ErrUnsupported when it is nil.
func (f Fake) RenderIndex(ctx context.Context, repo index.Repo, scope format.IndexScope, _ index.Reader) ([]format.Document, error) {
	if f.Render == nil {
		return nil, format.ErrUnsupported
	}
	return f.Render(ctx, repo, scope)
}

// Routes mounts the routes listed on Fake, then calls Mount when it is set.
func (f Fake) Routes(r format.Router, deps format.Deps) {
	r.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, "pong\n")
	})
	r.HandleFunc("GET /{repo}/ping", func(w http.ResponseWriter, req *http.Request) {
		repo, ok := format.RepoFrom(req.Context())
		if !ok { // the repo middleware did not run
			format.WriteError(w, errors.New("formattest: no repo in the request context"))
			return
		}
		writeText(w, "pong "+repo.Format+"/"+repo.Name+"\n")
	})
	r.HandleFunc("GET /{repo}/fail/{kind}", func(w http.ResponseWriter, req *http.Request) {
		format.WriteError(w, failure(req.PathValue("kind")))
	})
	if f.Mount != nil {
		f.Mount(r, deps)
	}
}

// failures are the errors of the fail route, by kind.
var failures = map[string]error{
	"invalid":         format.ErrInvalid,
	"unauthenticated": format.ErrUnauthenticated,
	"forbidden":       format.ErrForbidden,
	"notfound":        index.ErrNotFound,
	"conflict":        index.ErrConflict,
	"unsupported":     format.ErrUnsupported,
	"unauthorized":    provider.ErrUnauthorized,
	"other":           errors.New("formattest: other failure"),
}

// failure returns the error the fail route answers for kind, wrapped as a plugin would wrap
// it; an unknown kind gives an error wrapping format.ErrInvalid.
func failure(kind string) error {
	err, ok := failures[kind]
	if !ok {
		return fmt.Errorf("%w: unknown failure kind %q", format.ErrInvalid, kind)
	}
	return fmt.Errorf("formattest: fail %s: %w", kind, err)
}

// writeText answers 200 with a plain-text body; write errors mean the client went away.
func writeText(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, body)
}
