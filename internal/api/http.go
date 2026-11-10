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
	// Tokens is the service behind the tokens of a user and the deploy tokens of a repo; nil
	// means one over the index, the audit writer, the clock and the logger of Repos.
	Tokens *auth.Tokens
	// Users is the service behind /api/v1/users; nil means one over the index, the audit
	// writer, the clock and the logger of Repos.
	Users *auth.Users
	// Bindings is the service behind /api/v1/bindings; nil means one over the index, the
	// formats, the audit writer, the clock and the logger of Repos.
	Bindings *auth.Bindings
	// Auth decides whether the request's principal may perform an action on a repo or on the
	// system (auth.System): serve sets auth.RBAC. nil allows everything the credential allows,
	// for the tests of the handlers.
	Auth auth.Authorizer
	// Reindex starts the runs of POST …/reindex; nil answers it 501.
	Reindex Reindexer
	// Deleter deletes the versions of DELETE …/packages/{name}/versions/{version}: the ingest
	// pipeline; nil answers it 501.
	Deleter Deleter
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
//	POST   /api/v1/repos/{format}/{repo}/reindex       start a reindex of the repo: 202, 409 when one runs
//	GET    /api/v1/repos/{format}/{repo}/reindex       the latest run that covered the repo, with its report
//	DELETE /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}[?variant=]  delete a version, its referrers first
//	GET    /api/v1/repos/{format}/{repo}/deploy-tokens       the deploy tokens of a repo
//	POST   /api/v1/repos/{format}/{repo}/deploy-tokens       create one; the answer has its secret
//	DELETE /api/v1/repos/{format}/{repo}/deploy-tokens/{id}  revoke one
//	GET    /api/v1/users[?limit=&cursor=]              list the live users, a page at a time
//	POST   /api/v1/users                               create a user
//	GET    /api/v1/users/{user}                        one user, by name or uid
//	PATCH  /api/v1/users/{user}                        change its name and disabled
//	DELETE /api/v1/users/{user}                        delete it
//	POST   /api/v1/users/{user}/password               set its password
//	GET    /api/v1/users/{user}/tokens                 the personal access tokens of a user
//	POST   /api/v1/users/{user}/tokens                 create one; the answer has its secret
//	DELETE /api/v1/users/{user}/tokens/{id}            revoke one
//	GET    /api/v1/bindings[?principal=&limit=&cursor=]  list the bindings, a page at a time
//	POST   /api/v1/bindings                            create a binding
//	DELETE /api/v1/bindings/{id}                       delete one
//	GET    /api/v1/audit[?repo=&principal=&action=&since=&until=&limit=&cursor=]  the audit log, newest first
//	GET    /api/v1/whoami                              the request's principal
//	GET    /api/v1/openapi.yaml                        the OpenAPI document of these routes
//
// Every other method and path under /api/ is a JSON NOT_FOUND. Answers are JSON;
// errors are the envelope of §4. New panics when o.Repos is nil.
func New(o Options) http.Handler {
	if o.Repos == nil {
		panic("api: New: nil Options.Repos")
	}
	h := &handler{repos: o.Repos, tokens: o.Tokens, users: o.Users, bindings: o.Bindings, auth: o.Auth, reindex: o.Reindex,
		deleter: o.Deleter, log: o.Log}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.tokens == nil {
		h.tokens = &auth.Tokens{Index: o.Repos.Index, Audit: o.Repos.Audit, Now: o.Repos.Now, Log: h.log}
	}
	if h.users == nil {
		h.users = &auth.Users{Index: o.Repos.Index, Audit: o.Repos.Audit, Now: o.Repos.Now, Log: h.log}
	}
	if h.bindings == nil {
		h.bindings = &auth.Bindings{Index: o.Repos.Index, Formats: o.Repos.Formats, Audit: o.Repos.Audit, Now: o.Repos.Now, Log: h.log}
	}
	mux := http.NewServeMux()
	for _, rt := range h.routes() {
		mux.Handle(rt.pattern, h.serve(rt.fn))
	}
	mux.Handle("GET /api/v1/openapi.yaml", http.HandlerFunc(serveOpenAPI))
	mux.Handle("/api/", h.serve(h.notFound))
	return mux
}

// route is a method and path of the API, as http.ServeMux takes them, and its endpoint.
type route struct {
	pattern string
	fn      endpoint
}

// routes are the endpoints of the API: what New serves and what docs/api/openapi.yaml
// describes (TestOpenAPIRoutes holds the two to each other).
func (h *handler) routes() []route {
	return []route{
		{"GET /api/v1/repos", h.listRepos},
		{"POST /api/v1/repos", h.createRepo},
		{"GET /api/v1/repos/{format}/{repo}", h.getRepo},
		{"PATCH /api/v1/repos/{format}/{repo}", h.updateRepo},
		{"DELETE /api/v1/repos/{format}/{repo}", h.deleteRepo},
		{"POST /api/v1/repos/{format}/{repo}/reindex", h.reindexRepo},
		{"GET /api/v1/repos/{format}/{repo}/reindex", h.reindexStatus},
		{"DELETE /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}", h.deleteVersion},
		{"GET /api/v1/repos/{format}/{repo}/deploy-tokens", h.listDeployTokens},
		{"POST /api/v1/repos/{format}/{repo}/deploy-tokens", h.createDeployToken},
		{"DELETE /api/v1/repos/{format}/{repo}/deploy-tokens/{id}", h.revokeDeployToken},
		{"GET /api/v1/users", h.listUsers},
		{"POST /api/v1/users", h.createUser},
		{"GET /api/v1/users/{user}", h.getUser},
		{"PATCH /api/v1/users/{user}", h.updateUser},
		{"DELETE /api/v1/users/{user}", h.deleteUser},
		{"POST /api/v1/users/{user}/password", h.setPassword},
		{"GET /api/v1/users/{user}/tokens", h.listTokens},
		{"POST /api/v1/users/{user}/tokens", h.createToken},
		{"DELETE /api/v1/users/{user}/tokens/{id}", h.revokeToken},
		{"GET /api/v1/bindings", h.listBindings},
		{"POST /api/v1/bindings", h.createBinding},
		{"DELETE /api/v1/bindings/{id}", h.deleteBinding},
		{"GET /api/v1/audit", h.listAudit},
		{"GET /api/v1/whoami", h.whoami},
	}
}

// handler serves the management API.
type handler struct {
	repos    *Repos
	tokens   *auth.Tokens
	users    *auth.Users
	bindings *auth.Bindings
	auth     auth.Authorizer
	reindex  Reindexer
	deleter  Deleter
	log      *slog.Logger
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
// Location. The admin role is checked before the body is read, and must stay first: the 409 of
// a taken name, and the 400 of a body that does not parse, then reach an admin only, who sees
// every repo, and nobody else learns by trying names which repos exist (docs/spec/rbac.md §3).
func (h *handler) createRepo(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allow(r, auth.ActionManage, auth.System, auth.System); err != nil {
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

// notFound answers every other method and path under /api/.
func (h *handler) notFound(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	return 0, nil, &clientError{
		msg: fmt.Sprintf("%s %s is not an endpoint of this version (docs/api.md)",
			r.Method, r.URL.Path),
		err: index.ErrNotFound,
	}
}

// allow returns nil when the request's principal may perform a on formatName/repo. It is the
// one place where the API asks the Authorizer, through format.Authorize, so that a denial
// follows docs/spec/rbac.md §3 as on the frontends: the anonymous principal gets
// format.ErrUnauthenticated (401 with a Basic challenge), a principal that can read the repo
// but lacks a gets format.ErrForbidden (403), and one that cannot read it index.ErrNotFound
// (404), the answer of a repo that does not exist. Reading a repo and listing it need read;
// changing, deleting and reindexing it need manage.
//
// Creating a repo is system-level, and asks for manage on auth.System/auth.System, which only
// the admin role has (rbac.md §2): a maintainer on */* has every repo and not the system. An
// authenticated principal that is denied gets 403, never 404: the check comes before the body
// names a repo (createRepo), so no repo's existence is at stake.
//
// The request's credential limits the principal first, with or without an Authorizer
// (format.Authorize, auth.Credential.Limit): a deploy token and a personal access token with
// scopes never get manage, and a deploy token reaches its own repo only.
func (h *handler) allow(r *http.Request, a auth.Action, formatName, repo string) error {
	return h.authorize(r, format.Deps{Auth: h.auth}, a, formatName, repo)
}

// limit is allow without the Authorizer: what the request's credential allows alone.
func (h *handler) limit(r *http.Request, a auth.Action, formatName, repo string) error {
	return h.authorize(r, format.Deps{}, a, formatName, repo)
}

// authorize asks format.Authorize with deps, and answers a principal denied on the system
// with format.ErrForbidden.
func (h *handler) authorize(r *http.Request, deps format.Deps, a auth.Action, formatName, repo string) error {
	err := format.Authorize(r.Context(), deps, a, index.Repo{Format: formatName, Name: repo})
	if formatName == auth.System && repo == auth.System && errors.Is(err, index.ErrNotFound) {
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
		{"kind", "it is set when the repo is created", b.Kind},
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

// clientIP is the address of the client without the port: the server's (server.ClientAddr,
// which believes X-Forwarded-For from server.trustedProxies only, ADR 0011), or the peer of the
// connection for a request that did not come through the server's middleware.
func clientIP(r *http.Request) string {
	if addr := server.ClientAddr(r.Context()); addr != "" {
		return addr
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
