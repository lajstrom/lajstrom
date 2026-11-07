package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// Options configures the handler New returns.
type Options struct {
	// Repos is the service behind /api/v1/repos; required.
	Repos *Repos
	// Auth decides whether the request's principal may perform an action on a repo. nil allows
	// everything: there is no authentication in this version, and every endpoint is open until
	// RBAC (#45) sets it.
	Auth auth.Authorizer
	// Log receives the causes of the 5xx answers, with the request id; nil means
	// slog.Default().
	Log *slog.Logger
}

// maxBody is the largest request body the API reads; a larger one is answered 413.
const maxBody = 64 << 10

// New returns the handler of the management API, which the server mounts at /api/
// (server.Options.API). Its routes (docs/api.md):
//
//	GET    /api/v1/repos[?format=<f>]                  list the repos
//	POST   /api/v1/repos                               create a repo
//	GET    /api/v1/repos/{format}/{repo}               one repo
//	PATCH  /api/v1/repos/{format}/{repo}               change anonymousRead and policy
//	DELETE /api/v1/repos/{format}/{repo}[?force=true]  delete a repo
//	POST   /api/v1/repos/{format}/{repo}/reindex       501 until reindex comes with #29
//
// Every other method and path under /api/ is a JSON NOT_FOUND: the users, tokens, bindings and
// whoami endpoints of docs/spec/http-routing.md §4 come with #47, audit with #46. Answers are
// JSON; errors are the envelope of §4. New panics when o.Repos is nil.
func New(o Options) http.Handler {
	if o.Repos == nil {
		panic("api: New: nil Options.Repos")
	}
	h := &handler{repos: o.Repos, auth: o.Auth, log: o.Log}
	if h.log == nil {
		h.log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/repos", h.serve(h.listRepos))
	mux.Handle("POST /api/v1/repos", h.serve(h.createRepo))
	mux.Handle("GET /api/v1/repos/{format}/{repo}", h.serve(h.getRepo))
	mux.Handle("PATCH /api/v1/repos/{format}/{repo}", h.serve(h.updateRepo))
	mux.Handle("DELETE /api/v1/repos/{format}/{repo}", h.serve(h.deleteRepo))
	mux.Handle("POST /api/v1/repos/{format}/{repo}/reindex", h.serve(h.reindexRepo))
	mux.Handle("/api/", h.serve(h.notFound))
	return mux
}

// handler serves the management API.
type handler struct {
	repos *Repos
	auth  auth.Authorizer
	log   *slog.Logger
}

// endpoint is a handler that returns its answer: the status and the value to encode as JSON
// (nil for no body), or an error, which serve answers with the envelope.
type endpoint func(w http.ResponseWriter, r *http.Request) (status int, body any, err error)

// serve adapts an endpoint to http.Handler.
func (h *handler) serve(fn endpoint) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body, err := fn(w, r)
		switch {
		case err != nil:
			h.fail(w, r, err)
		case body == nil:
			w.WriteHeader(status)
		default:
			h.writeJSON(w, r, status, body)
		}
	})
}

// listRepos answers the repos the principal may read, of the format of ?format or of every
// format, as {"repos": […]} ordered by format and name.
func (h *handler) listRepos(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	repos, err := h.repos.List(r.Context(), r.URL.Query().Get("format"))
	if err != nil {
		return 0, nil, err
	}
	if repos, err = h.readable(r, repos); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, RepoList{Repos: repos}, nil
}

// readable keeps the repos the principal may read: read lists a repo in /api/v1/repos
// (docs/spec/rbac.md §2). A denial leaves the repo out, so that the list does not tell which
// repos exist; an Authorizer that cannot decide fails the whole list.
func (h *handler) readable(r *http.Request, repos []Repo) ([]Repo, error) {
	kept := repos[:0]
	for _, repo := range repos {
		switch err := h.allow(r, auth.ActionRead, repo.Format, repo.Name); {
		case err == nil:
			kept = append(kept, repo)
		case !denied(err):
			return nil, err
		}
	}
	return kept, nil
}

// denied reports whether err is a denial of allow rather than a failure to decide.
func denied(err error) bool {
	return errors.Is(err, format.ErrUnauthenticated) || errors.Is(err, format.ErrForbidden) ||
		errors.Is(err, index.ErrNotFound)
}

// createRepo creates the repo of the CreateRequest in the body: 201 with the resource and its
// Location.
func (h *handler) createRepo(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allow(r, auth.ActionManage, systemMask, systemMask); err != nil {
		return 0, nil, err
	}
	var req CreateRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	repo, err := h.repos.Create(r.Context(), req, origin(r))
	if err != nil {
		return 0, nil, err
	}
	w.Header().Set("Location", "/api/v1/repos/"+repo.Format+"/"+repo.Name)
	return http.StatusCreated, repo, nil
}

// getRepo answers one repo.
func (h *handler) getRepo(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionRead, f, name); err != nil {
		return 0, nil, err
	}
	repo, err := h.repos.Get(r.Context(), f, name)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, repo, nil
}

// updateRepo applies the UpdateRequest of the body and answers the repo as it is now.
func (h *handler) updateRepo(w http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return 0, nil, err
	}
	var body patchBody
	if err := decodeBody(w, r, &body); err != nil {
		return 0, nil, err
	}
	req, err := body.request()
	if err != nil {
		return 0, nil, err
	}
	repo, err := h.repos.Update(r.Context(), f, name, req, origin(r))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, repo, nil
}

// deleteRepo deletes a repo, refusing one with versions unless ?force=true: 204.
func (h *handler) deleteRepo(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return 0, nil, err
	}
	force, err := boolParam(r, "force")
	if err != nil {
		return 0, nil, err
	}
	if err := h.repos.Delete(r.Context(), f, name, force, origin(r)); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// reindexRepo takes the path of the reindex of one repo now, so that it is not taken by
// anything else: 404 for a repo that does not exist, 501 until reindex comes with #29.
func (h *handler) reindexRepo(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return 0, nil, err
	}
	if _, err := h.repos.Get(r.Context(), f, name); err != nil {
		return 0, nil, err
	}
	return 0, nil, &clientError{msg: "reindex comes with #29", err: format.ErrUnsupported}
}

// notFound answers every other method and path under /api/.
func (h *handler) notFound(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	return 0, nil, &clientError{
		msg: fmt.Sprintf("%s %s is not in this version; users, tokens, bindings and whoami come with #47, audit with #46",
			r.Method, r.URL.Path),
		err: index.ErrNotFound,
	}
}

// systemMask is the mask segment that matches everything; manage on */* is what the admin
// binding grants.
const systemMask = "*"

// allow returns nil when the request's principal may perform a on formatName/repo. It is the
// one place where the API asks the Authorizer, through format.Authorize, so that a denial
// follows docs/spec/rbac.md §3 as on the frontends: the anonymous principal gets
// format.ErrUnauthenticated (401 with a Basic challenge), a principal that can read the repo
// but lacks a gets format.ErrForbidden (403), and one that cannot read it index.ErrNotFound
// (404), the answer of a repo that does not exist. Reading a repo and listing it need read;
// changing, deleting and reindexing it need manage.
//
// Creating a repo is system-level, and asks for manage on */*: the admin binding matches every
// repo, and #45 settles the system resource of rbac.md §2. No repo's existence is at stake
// there, so an authenticated principal that is denied gets 403, never 404.
func (h *handler) allow(r *http.Request, a auth.Action, formatName, repo string) error {
	if h.auth == nil {
		return nil // TODO(#45): RBAC sets Options.Auth; until then every endpoint is open.
	}
	err := format.Authorize(r.Context(), format.Deps{Auth: h.auth}, a, index.Repo{Format: formatName, Name: repo})
	if formatName == systemMask && repo == systemMask && errors.Is(err, index.ErrNotFound) {
		return fmt.Errorf("%w: %s on the system", format.ErrForbidden, a)
	}
	return err
}

// patchBody is the body of PATCH: the fields of an UpdateRequest, and those that cannot
// change, refused by name rather than as unknown fields.
type patchBody struct {
	AnonymousRead *bool           `json:"anonymousRead"`
	Policy        json.RawMessage `json:"policy"`
	Format        json.RawMessage `json:"format"`
	Name          json.RawMessage `json:"name"`
	Registry      json.RawMessage `json:"registry"`
	Kind          json.RawMessage `json:"kind"`
}

// request returns the UpdateRequest of b, or an error naming a field that cannot change.
func (b *patchBody) request() (UpdateRequest, error) {
	for _, f := range []struct {
		field, why string
		value      json.RawMessage
	}{
		{"format", "it identifies the repo", b.Format},
		{"name", "it identifies the repo", b.Name},
		{"registry", "the artifacts stay in the registry they were pushed to, and moving the repo would orphan them", b.Registry},
		{"kind", "every repo is hosted in this version", b.Kind},
	} {
		if f.value != nil {
			return UpdateRequest{}, invalid("%s: cannot change; %s", f.field, f.why)
		}
	}
	return UpdateRequest{AnonymousRead: b.AnonymousRead, Policy: b.Policy}, nil
}

// decodeBody reads the JSON object of the request body into v: at most maxBody bytes, no field
// that v does not have, nothing after the object. Its errors are for the client and name the
// field when there is one; a body that is too large is a *http.MaxBytesError (413).
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return bodyError(err)
	}
	switch _, err := dec.Token(); {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return bodyError(err)
	}
	return invalid("body: must be one JSON object, with nothing after it")
}

// bodyError turns an error of decodeBody into one for the client.
func bodyError(err error) error {
	var (
		tooLarge *http.MaxBytesError
		typeErr  *json.UnmarshalTypeError
	)
	switch {
	case errors.As(err, &tooLarge):
		return &clientError{msg: fmt.Sprintf("body: larger than %d bytes", maxBody), err: err}
	case errors.Is(err, io.EOF):
		return invalid("body: must be a JSON object")
	case errors.As(err, &typeErr) && typeErr.Field != "":
		return invalid("%s: must not be a JSON %s", typeErr.Field, typeErr.Value)
	case errors.As(err, &typeErr):
		return invalid("body: must be a JSON object, not %s", typeErr.Value)
	}
	return invalid("body: %s", strings.TrimPrefix(err.Error(), "json: "))
}

// boolParam returns the query parameter key as a bool; absent is false.
func boolParam(r *http.Request, key string) (bool, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, invalid("%s: %q is not true or false", key, v)
	}
	return b, nil
}

// origin is the Origin of a change made by a request.
func origin(r *http.Request) Origin {
	return Origin{Source: SourceAPI, IP: clientIP(r), RequestID: server.RequestID(r.Context())}
}

// clientIP is the address of the client without the port. X-Forwarded-For, from trusted
// proxies only, comes with #50.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
