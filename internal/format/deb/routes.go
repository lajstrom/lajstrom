package deb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// The signed documents of a suite and the keyring of a repo (docs/spec/http-routing.md §3,
// apt). The index_cache kinds are the file names below dists/<suite>/.
const (
	kindInRelease  = "InRelease"
	kindReleaseGPG = "Release.gpg"

	signatureContentType = "application/pgp-signature"
	keysContentType      = "application/pgp-keys"

	// signRounds bounds the rounds of handlers.signed; a round is lost when the suite is
	// rendered again between the read of its Release and the store of the signatures.
	signRounds = 3
)

// Routes mounts the routes of the apt frontend below /apt/ (docs/spec/http-routing.md §3,
// apt): a GET (and so a HEAD) that needs read on the repo, or the PUT of an upload, which
// needs write:
//
//	GET /{repo}/dists/{suite}/Release                                the Release of the suite
//	GET /{repo}/dists/{suite}/InRelease, Release.gpg                 the Release signed with the repo's keys
//	GET /{repo}/dists/{suite}/{component}/binary-{arch}/Packages     and Packages.gz, Packages.xz
//	GET /{repo}/dists/{suite}/{component}/binary-{arch}/by-hash/SHA256/{hash}
//	GET /{repo}/pool/{component}/{p}/{source}/{filename}             a .deb, through format.ServeBlob
//	GET /{repo}/key.asc                                              the keyring of the repo, for signed-by=
//	PUT /{repo}/upload?suite=&component=[&arch=]                     one .deb, published through deps.Ingest
//	POST /{repo}/upload?suite=&component=                            a source package, multipart, through deps.Sessions
//	GET /{repo}/dists/{suite}/{component}/source/Sources             and Sources.gz, Sources.xz, by the route of Packages
//	GET /{repo}/dists/{suite}/{component}/Contents-{arch}            and Contents-{arch}.gz
//	GET /{repo}/dists/{suite}/{component}/by-hash/SHA256/{hash}      a Contents of the component by its hash
//
// The documents come from index_cache through format.CachedDocument and are served with
// format.ServeDocument (ETag, Last-Modified, If-None-Match, Range). deps.Signer signs; when it
// is nil (the server has no master key) InRelease, Release.gpg and key.asc are 404 and clients
// take the unsigned Release from a source marked [trusted=yes]. A package reaches an apt repo
// through the upload route or by oras push through the facade, whose hook runs Validate; ingest
// invalidates the repo's documents either way, and the next GET renders and signs the suite
// again, so nothing here runs after a publish. The handlers render with a Plugin that keeps
// the providers of deps (RenderIndex reads a metadata layer the index left out) and the clock
// of p.
func (p Plugin) Routes(r format.Router, deps format.Deps) {
	if deps.Log == nil {
		deps.Log = slog.Default().With("format", Name)
	}
	svc := &services{providers: deps.Providers}
	if p.svc != nil {
		svc.now = p.svc.now
	}
	h := &handlers{deps: deps, plugin: Plugin{svc: svc}}
	r.HandleFunc("GET /{repo}/dists/{suite}/{file}", h.release)
	r.HandleFunc("GET /{repo}/dists/{suite}/{component}/{file}", h.packages)
	r.HandleFunc("GET /{repo}/dists/{suite}/{component}/{dir}/{file}", h.packages)
	r.HandleFunc("GET /{repo}/dists/{suite}/{component}/by-hash/SHA256/{hash}", h.byHash)
	r.HandleFunc("GET /{repo}/dists/{suite}/{component}/{dir}/by-hash/SHA256/{hash}", h.byHash)
	r.HandleFunc("GET /{repo}/pool/{component}/{p}/{source}/{filename}", h.pool)
	r.HandleFunc("GET /{repo}/key.asc", h.publicKeys)
	r.HandleFunc("PUT /{repo}/upload", h.upload)
	r.HandleFunc("POST /{repo}/upload", h.uploadSource)
}

// handlers are the apt routes over one format.Deps.
type handlers struct {
	deps   format.Deps
	plugin Plugin
}

// start returns the repo of the request once the principal may read it; otherwise it answers
// the error and ok is false.
func (h *handlers) start(w http.ResponseWriter, r *http.Request) (repo index.Repo, ok bool) {
	return h.startAs(w, r, auth.ActionRead)
}

// startAs returns the repo of the request once the principal may perform a on it; otherwise it
// answers the error and ok is false.
func (h *handlers) startAs(w http.ResponseWriter, r *http.Request, a auth.Action) (repo index.Repo, ok bool) {
	repo, ok = format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("deb: the route has no repo"))
		return index.Repo{}, false
	}
	if err := format.Authorize(r.Context(), h.deps, a, repo); err != nil {
		h.fail(w, r, err)
		return index.Repo{}, false
	}
	return repo, true
}

// fail answers err with format.WriteError and logs it when the status is a 5xx, whose cause the
// client does not see.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	if status := format.WriteError(w, err); status >= http.StatusInternalServerError {
		h.deps.Log.Log(r.Context(), server.FailLevel(r), "apt: request failed", "method", r.Method, "path", r.URL.EscapedPath(),
			"status", status, "error", err.Error())
	}
}

// release serves dists/{suite}/Release and its signed forms, InRelease (clearsigned) and
// Release.gpg (the detached signature), made from the bytes of the Release in index_cache
// (signed). Without a Signer those two are 404, and apt takes the unsigned Release from a
// source marked [trusted=yes].
func (h *handlers) release(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r)
	if !ok {
		return
	}
	suite, file := r.PathValue("suite"), r.PathValue("file")
	switch file {
	case kindRelease:
		h.serve(w, r, repo, suite, kindRelease)
	case kindInRelease, kindReleaseGPG:
		if h.deps.Signer == nil {
			h.fail(w, r, unsigned(repo, "dists/"+suite+"/"+file))
			return
		}
		c, err := h.signed(r.Context(), repo, suite, file)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		format.ServeDocument(w, r, lastSigned(c), contentType(file))
	default:
		h.fail(w, r, notFound(repo, suite, file))
	}
}

// publicKeys serves key.asc: the armored keyring of the repo from the Signer, its active key
// (made on the first request) and the keys retired within keys.retireGrace, what signed-by=
// points at. It is not cached: it is small, and a rotation changes it. Without a Signer it is
// 404.
func (h *handlers) publicKeys(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r)
	if !ok {
		return
	}
	if h.deps.Signer == nil {
		h.fail(w, r, unsigned(repo, "key.asc"))
		return
	}
	b, err := h.deps.Signer.PublicKeys(r.Context(), repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sum := sha256.Sum256(b)
	format.ServeDocument(w, r, &index.Cache{Content: b, SHA256: hex.EncodeToString(sum[:])}, keysContentType)
}

// signed returns the InRelease or the Release.gpg of the suite, kind. Both are made from the
// bytes of the Release of the suite in index_cache, rendered first when it is not there, and
// stored beside it with its RenderedAt and SignedAt the time of signing; a signed document
// whose RenderedAt is not that of the Release is of an older render and is made again, and so
// is one that the Signer no longer calls current: a key that signed it has stopped signing,
// its grace (keys.retireGrace) over, which no write marks (#577). The
// Signer signs outside any transaction of the index: it reads the repo's keys and makes the
// first one, and in the transaction of format.CachedDocument that read would wait for the
// connection the transaction holds (SQLite in memory has one) and the insert for its write
// lock. The documents are stored only while the Release they sign is the one in index_cache,
// so a render that replaced it meanwhile is signed in the next round.
func (h *handlers) signed(ctx context.Context, repo index.Repo, suite, kind string) (*index.Cache, error) {
	for range signRounds {
		rel, err := h.cached(ctx, repo, suite, kindRelease)
		if err != nil {
			return nil, err
		}
		c, err := h.deps.Index.GetIndexCache(ctx, repo.ID, suite, kind)
		switch {
		case err == nil && c.RenderedAt.Equal(rel.RenderedAt):
			current, err := h.deps.Signer.Current(ctx, repo, c.Content)
			if err != nil {
				return nil, err
			}
			if current {
				return c, nil
			}
		case err != nil && !errors.Is(err, index.ErrNotFound):
			return nil, err
		}
		in, err := h.deps.Signer.ClearSign(ctx, repo, rel.Content)
		if err != nil {
			return nil, err
		}
		sig, err := h.deps.Signer.DetachSign(ctx, repo, rel.Content)
		if err != nil {
			return nil, err
		}
		c, err = h.storeSigned(ctx, rel, kind, map[string][]byte{kindInRelease: in, kindReleaseGPG: sig})
		if !errors.Is(err, errRenderedAgain) {
			return c, err
		}
	}
	return nil, fmt.Errorf("deb: apt/%s dists/%s/%s: the Release was rendered again while it was signed, %d times",
		repo.Name, suite, kind, signRounds)
}

// lastSigned returns the signed document c with its SignedAt as its RenderedAt, the
// Last-Modified of format.ServeDocument: a document signed again without a new render is newer
// than the Release it signs, and apt, which asks with If-Modified-Since, fetches it.
func lastSigned(c *index.Cache) *index.Cache {
	if c.SignedAt == nil || !c.SignedAt.After(c.RenderedAt) {
		return c
	}
	served := *c
	served.RenderedAt = *c.SignedAt
	return &served
}

// errRenderedAgain is the error of storeSigned when the Release it signed is no longer the one
// in index_cache, or an invalidation came while it stored them.
var errRenderedAgain = errors.New("deb: the Release was rendered again")

// storeSigned stores the signed documents of the Release rel, by kind, in one transaction that
// first reads the repo's IndexCacheGeneration and checks that rel is still the Release in
// index_cache, and returns the one of kind.
func (h *handlers) storeSigned(ctx context.Context, rel *index.Cache, kind string, docs map[string][]byte) (*index.Cache, error) {
	var found *index.Cache
	err := h.deps.Index.Tx(ctx, func(tx index.Writer) error {
		gen, err := tx.IndexCacheGeneration(ctx, rel.RepoID)
		if err != nil {
			return err
		}
		cur, err := tx.GetIndexCache(ctx, rel.RepoID, rel.Scope, kindRelease)
		switch {
		case errors.Is(err, index.ErrNotFound):
			return errRenderedAgain
		case err != nil:
			return err
		case cur.SHA256 != rel.SHA256 || !cur.RenderedAt.Equal(rel.RenderedAt):
			return errRenderedAgain
		}
		signedAt := h.plugin.clock().UTC()
		for _, k := range []string{kindInRelease, kindReleaseGPG} {
			sum := sha256.Sum256(docs[k])
			c := &index.Cache{RepoID: rel.RepoID, Scope: rel.Scope, Kind: k, Content: docs[k], SHA256: hex.EncodeToString(sum[:]),
				RenderedAt: rel.RenderedAt, SignedAt: &signedAt}
			switch stored, err := tx.PutIndexCache(ctx, c, gen); {
			case err != nil:
				return err
			case !stored:
				return errRenderedAgain
			}
			if k == kind {
				found = c
			}
		}
		return nil
	})
	return found, err
}

// unsigned is the error for a signed document or the keyring of a repo when the server does
// not sign.
func unsigned(repo index.Repo, path string) error {
	return fmt.Errorf("%w: apt/%s/%s: this server does not sign, keys.masterKeyEnv is not set; use deb [trusted=yes]",
		index.ErrNotFound, repo.Name, path)
}

// packages serves dists/{suite}/{component}/{dir}/{file}, a Packages or Sources and their
// compressed forms, and dists/{suite}/{component}/{file}, a Contents-<arch> or its .gz: an index
// that the Release of the suite lists.
func (h *handlers) packages(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r)
	if !ok {
		return
	}
	h.serve(w, r, repo, r.PathValue("suite"), indexDir(r)+r.PathValue("file"))
}

// indexDir returns the directory of an index of the request below dists/<suite>/, with its
// trailing "/": <component>/<dir>/ for a Packages or a Sources, <component>/ for a Contents.
func indexDir(r *http.Request) string {
	dir := r.PathValue("component") + "/"
	if d := r.PathValue("dir"); d != "" {
		dir += d + "/"
	}
	return dir
}

// serve answers the document kind of the suite. The Release comes first, from index_cache or
// rendered with the whole suite, and a kind it does not list is 404 without a render: the
// documents of a suite are stored together, so the Release is the list of what the cache holds,
// and a request for a component or an architecture that the suite does not have does not
// render the suite again on every try.
func (h *handlers) serve(w http.ResponseWriter, r *http.Request, repo index.Repo, suite, kind string) {
	c, err := h.cached(r.Context(), repo, suite, kindRelease)
	if err == nil && kind != kindRelease {
		if _, ok := listed(c.Content)[kind]; !ok {
			err = notFound(repo, suite, kind)
		} else {
			c, err = h.cached(r.Context(), repo, suite, kind)
		}
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	format.ServeDocument(w, r, c, contentType(kind))
}

// byHash serves dists/{suite}/{component}/{dir}/by-hash/SHA256/{hash} and
// dists/{suite}/{component}/by-hash/SHA256/{hash}: the index of that directory, a Packages, a
// Sources or a Contents in any of their forms, whose SHA256 the current Release lists as hash
// (apt asks for it so when the Release says Acquire-By-Hash: yes). A hash of an older render is
// 404, and apt falls back to the file by its name.
func (h *handlers) byHash(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r)
	if !ok {
		return
	}
	suite, hash, dir := r.PathValue("suite"), r.PathValue("hash"), indexDir(r)
	rel, err := h.cached(r.Context(), repo, suite, kindRelease)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	kind := ""
	for path, sum := range listed(rel.Content) {
		if sum == hash && strings.HasPrefix(path, dir) && !strings.Contains(path[len(dir):], "/") {
			kind = path
		}
	}
	var c *index.Cache
	if kind != "" {
		c, err = h.cached(r.Context(), repo, suite, kind)
	}
	switch {
	case err != nil:
		h.fail(w, r, err)
	case c == nil || c.SHA256 != hash: // not listed, or rendered again since the Release was read
		h.fail(w, r, fmt.Errorf("%w: apt/%s dists/%s/%s has no index with the SHA256 %s in the current Release",
			index.ErrNotFound, repo.Name, suite, strings.TrimSuffix(dir, "/"), hash))
	default:
		format.ServeDocument(w, r, c, contentType(kind))
	}
}

// cached returns the document kind of the suite from index_cache, rendered with the whole
// suite on a miss (format.CachedDocument). Nothing here invalidates: ingest and delete
// invalidate every document of the repo in the transaction that changes its versions, so the
// first GET after a publish renders the suite again. On Postgres a render that read the
// versions before such a commit can store documents without the new version until the next
// invalidation; format.CachedDocument says so, and the replicas criteria (A-09) need that
// window closed there.
func (h *handlers) cached(ctx context.Context, repo index.Repo, suite, kind string) (*index.Cache, error) {
	return format.CachedDocument(ctx, h.deps.Index, h.plugin, repo, format.IndexScope(suite), kind)
}

// notFound is the error for a document a suite does not have.
func notFound(repo index.Repo, suite, kind string) error {
	return fmt.Errorf("%w: apt/%s has no dists/%s/%s", index.ErrNotFound, repo.Name, suite, kind)
}
