package generic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
)

func TestMain(m *testing.M) {
	// No waiting between retries: the retry tests count requests, they do not time them.
	retryWait.min, retryWait.max = 0, 0
	os.Exit(m.Run())
}

// fakeRegistry is a minimal OCI distribution API for the unit tests: /v2/, _catalog with Link
// pagination, manifests, blobs and uploads, tags, referrers, and Basic or bearer auth. It logs
// every request. The knobs are set before the first request.
type fakeRegistry struct {
	t   *testing.T
	srv *httptest.Server

	referrers      bool   // serve the referrers API; otherwise its path is a plain 404
	deleteDisabled bool   // answer DELETE with 405 UNSUPPORTED, like distribution without delete
	catalogStatus  int    // answer _catalog with this status instead of the listing
	catalogMode    string // "": paginate repos; "repeat": the first page again and again; "endless": a new page every time
	redirect       string // answer blob GETs and HEADs with 307 to redirect + "/" + digest
	stateful       bool   // upload Locations are absolute and carry a _state that changes with every chunk and must come back, as distribution's
	refuseStreamed int    // answer a chunk without a known length and Content-Range into a session that holds bytes with this status, as Gitea (400) and JFrog (416); 0 takes it
	chunkLimit     int    // as GHCR: a PATCH with Content-Range of more than chunkLimit bytes is 416, and one without Content-Range starts the session over; 0 takes any
	authMode       string // "", "basic" or "bearer"
	challengeScope string // the scope parameter of the bearer challenge; Gitea sends "*" whatever the request
	user, pass     string // the credentials auth accepts
	token          string // the bearer token /token issues
	realm          string // the realm of the bearer challenge; empty: the fake's own /token

	// hook sees every request first; it returns true when it answered it.
	hook func(w http.ResponseWriter, r *http.Request) bool

	mu        sync.Mutex
	requests  []string                            // "GET /v2/…?query"
	auths     []string                            // the Authorization header of each request
	repos     []string                            // the catalog, in addition to the repositories with content
	manifests map[string]map[string]*fakeManifest // repo → digest or tag → manifest
	blobs     map[string]map[digest.Digest][]byte // repo → digest → content
	uploads   map[string]*fakeUpload              // upload id → session
	uploadSeq int
	streams   map[digest.Digest]func(w http.ResponseWriter) // blobs a GET answers with this function
}

// fakeManifest is a stored manifest.
type fakeManifest struct {
	desc    ocispec.Descriptor
	content []byte
	subject digest.Digest
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{
		t:         t,
		referrers: true,
		user:      "lajstrom",
		pass:      "pa55-word",
		token:     "t0ken-value",
		manifests: map[string]map[string]*fakeManifest{},
		blobs:     map[string]map[digest.Digest][]byte{},
		uploads:   map[string]*fakeUpload{},
		streams:   map[digest.Digest]func(http.ResponseWriter){},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// provider returns a Provider for the fake with prefix "art/" and the test's logger; edit
// changes the configuration first.
func (f *fakeRegistry) provider(t *testing.T, log *slog.Logger, edit func(*config.Registry)) *Provider {
	t.Helper()
	cfg := config.Registry{Name: "fake", Type: Type, URL: f.srv.URL, Prefix: "art/"}
	if edit != nil {
		edit(&cfg)
	}
	p, err := New(cfg, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// log returns the requests so far.
func (f *fakeRegistry) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// blobLog returns the requests so far but the probe's, which the first read of a provider sends
// too (readQuirks): the ping, the catalog probe and the referrers probe.
func (f *fakeRegistry) blobLog() []string {
	return slices.DeleteFunc(f.log(), func(r string) bool {
		return r == "GET /v2/" || r == "GET /v2/_catalog?n=1" || strings.HasSuffix(r, "/"+probeRepository+"/referrers/"+zeroDigest)
	})
}

// count returns how many requests of the log start with prefix.
func (f *fakeRegistry) count(prefix string) int {
	n := 0
	for _, r := range f.log() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeRegistry) serve(w http.ResponseWriter, r *http.Request) {
	line := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	f.mu.Lock()
	f.requests = append(f.requests, line)
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.mu.Unlock()

	if f.hook != nil && f.hook(w, r) {
		return
	}
	if r.URL.Path == "/token" {
		f.serveToken(w, r)
		return
	}
	if !f.authorized(w, r) {
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	switch {
	case !ok:
		http.NotFound(w, r)
	case path == "":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	case path == "_catalog":
		f.serveCatalog(w, r)
	default:
		f.serveRepo(w, r, path)
	}
}

// authorized checks the credentials of r under authMode and answers the challenge when they
// are missing or wrong.
func (f *fakeRegistry) authorized(w http.ResponseWriter, r *http.Request) bool {
	switch f.authMode {
	case "basic":
		if u, p, ok := r.BasicAuth(); ok && u == f.user && p == f.pass {
			return true
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
	case "bearer":
		if r.Header.Get("Authorization") == "Bearer "+f.token {
			return true
		}
		realm := f.realm
		if realm == "" {
			realm = f.srv.URL + "/token"
		}
		challenge := fmt.Sprintf(`Bearer realm=%q,service="fake"`, realm)
		if f.challengeScope != "" {
			challenge += fmt.Sprintf(",scope=%q", f.challengeScope)
		}
		w.Header().Set("WWW-Authenticate", challenge)
	default:
		return true
	}
	writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
	return false
}

// serveToken issues the bearer token for the right Basic credentials.
func (f *fakeRegistry) serveToken(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != f.user || p != f.pass {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "bad credentials")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": f.token})
}

func (f *fakeRegistry) serveCatalog(w http.ResponseWriter, r *http.Request) {
	if f.catalogStatus != 0 {
		writeErr(w, f.catalogStatus, "UNSUPPORTED", "no catalog")
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	last := r.URL.Query().Get("last")
	var page []string
	more := false
	switch f.catalogMode {
	case "repeat":
		page, more = []string{"art/raw/a/x", "art/raw/b/y"}, true
	case "endless":
		i, _ := strconv.Atoi(strings.TrimPrefix(last, "art/raw/endless/"))
		page, more = []string{fmt.Sprintf("art/raw/endless/%06d", i+1)}, true
	default:
		all := f.catalog()
		start, _ := slices.BinarySearch(all, last)
		if last != "" && start < len(all) && all[start] == last {
			start++
		}
		page = all[start:]
		if n > 0 && len(page) > n {
			page, more = page[:n], true
		}
	}
	if more && len(page) > 0 {
		next := fmt.Sprintf("/v2/_catalog?last=%s&n=%d", page[len(page)-1], n)
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string][]string{"repositories": page})
}

// catalog returns the sorted names of the repositories: the configured ones and those with
// content.
func (f *fakeRegistry) catalog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := slices.Clone(f.repos)
	for r := range f.manifests {
		all = append(all, r)
	}
	for r := range f.blobs {
		all = append(all, r)
	}
	slices.Sort(all)
	return slices.Compact(all)
}

// serveRepo serves the paths below /v2/<name>/.
func (f *fakeRegistry) serveRepo(w http.ResponseWriter, r *http.Request, path string) {
	for _, sep := range []string{"/manifests/", "/blobs/uploads/", "/blobs/", "/referrers/"} {
		if i := strings.LastIndex(path, sep); i > 0 {
			repo, rest := path[:i], path[i+len(sep):]
			switch sep {
			case "/manifests/":
				f.serveManifest(w, r, repo, rest)
			case "/blobs/uploads/":
				f.serveUpload(w, r, repo, rest)
			case "/blobs/":
				f.serveBlob(w, r, repo, digest.Digest(rest))
			case "/referrers/":
				f.serveReferrers(w, repo, digest.Digest(rest))
			}
			return
		}
	}
	if repo, ok := strings.CutSuffix(path, "/tags/list"); ok {
		f.serveTags(w, repo)
		return
	}
	http.NotFound(w, r)
}

func (f *fakeRegistry) serveManifest(w http.ResponseWriter, r *http.Request, repo, ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		content, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		var m struct {
			MediaType string              `json:"mediaType"`
			Subject   *ocispec.Descriptor `json:"subject"`
		}
		if err := json.Unmarshal(content, &m); err != nil {
			writeErr(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		fm := &fakeManifest{desc: ocispec.Descriptor{MediaType: r.Header.Get("Content-Type"), Digest: digest.FromBytes(content), Size: int64(len(content))}, content: content}
		if m.Subject != nil {
			fm.subject = m.Subject.Digest
			if f.referrers {
				w.Header().Set("OCI-Subject", m.Subject.Digest.String())
			}
		}
		if f.manifests[repo] == nil {
			f.manifests[repo] = map[string]*fakeManifest{}
		}
		f.manifests[repo][fm.desc.Digest.String()] = fm
		f.manifests[repo][ref] = fm
		w.Header().Set("Docker-Content-Digest", fm.desc.Digest.String())
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet, http.MethodHead:
		fm := f.manifests[repo][ref]
		if fm == nil {
			writeErr(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		w.Header().Set("Content-Type", fm.desc.MediaType)
		w.Header().Set("Docker-Content-Digest", fm.desc.Digest.String())
		w.Header().Set("Content-Length", strconv.Itoa(len(fm.content)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(fm.content)
		}
	case http.MethodDelete:
		if f.deleteDisabled {
			writeErr(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "The operation is unsupported.")
			return
		}
		fm := f.manifests[repo][ref]
		if fm == nil {
			writeErr(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		for k, v := range f.manifests[repo] {
			if v == fm {
				delete(f.manifests[repo], k)
			}
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// fakeUpload is an upload session of the fake registry.
type fakeUpload struct {
	repo  string
	data  []byte
	state int // the _state of the session's Location when stateful
}

func (f *fakeRegistry) serveUpload(w http.ResponseWriter, r *http.Request, repo, id string) {
	if r.Method == http.MethodPost && id == "" {
		f.startUpload(w, r, repo)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	up := f.uploads[id]
	switch {
	case up == nil || up.repo != repo:
		writeErr(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "blob upload unknown to registry")
		return
	case f.stateful && r.URL.Query().Get("_state") != strconv.Itoa(up.state):
		writeErr(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "blob upload invalid")
		return
	}
	switch r.Method {
	case http.MethodPatch:
		if !f.appendChunk(w, r, up) {
			return
		}
		up.state++
		f.answerSession(w, id, up, http.StatusAccepted)
	case http.MethodPut:
		if !f.appendChunk(w, r, up) {
			return
		}
		want := digest.Digest(r.URL.Query().Get("digest"))
		if got := digest.FromBytes(up.data); got != want {
			writeErr(w, http.StatusBadRequest, "DIGEST_INVALID", "digest mismatch")
			return
		}
		if f.blobs[repo] == nil {
			f.blobs[repo] = map[digest.Digest][]byte{}
		}
		f.blobs[repo][want] = up.data
		delete(f.uploads, id)
		w.Header().Set("Location", "/v2/"+repo+"/blobs/"+want.String())
		w.Header().Set("Docker-Content-Digest", want.String())
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		f.answerSession(w, id, up, http.StatusNoContent)
	case http.MethodDelete:
		delete(f.uploads, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// startUpload answers POST …/blobs/uploads/: a mount when ?mount= names a blob that ?from has,
// otherwise a new session. The caller does not hold f.mu.
func (f *fakeRegistry) startUpload(w http.ResponseWriter, r *http.Request, repo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := digest.Digest(r.URL.Query().Get("mount")); m != "" {
		if b, ok := f.blobs[r.URL.Query().Get("from")][m]; ok {
			if f.blobs[repo] == nil {
				f.blobs[repo] = map[digest.Digest][]byte{}
			}
			f.blobs[repo][m] = b
			w.Header().Set("Location", "/v2/"+repo+"/blobs/"+m.String())
			w.Header().Set("Docker-Content-Digest", m.String())
			w.WriteHeader(http.StatusCreated)
			return
		}
	}
	f.uploadSeq++
	id := strconv.Itoa(f.uploadSeq)
	up := &fakeUpload{repo: repo}
	f.uploads[id] = up
	f.answerSession(w, id, up, http.StatusAccepted)
}

// appendChunk appends the body to the session: at the session's end when the request has a
// Content-Range, which must start there (else 416). With refuseStreamed, a chunk of unknown
// length after the first write is refused; with chunkLimit, a PATCH is held to it (limitChunk).
// The caller holds f.mu.
func (f *fakeRegistry) appendChunk(w http.ResponseWriter, r *http.Request, up *fakeUpload) bool {
	if r.Method == http.MethodPatch && !f.limitChunk(w, r, up) {
		return false
	}
	known := r.ContentLength >= 0 && r.Header.Get("Content-Range") != ""
	if f.refuseStreamed != 0 && !known && r.ContentLength != 0 && len(up.data) > 0 {
		writeErr(w, f.refuseStreamed, "BLOB_UPLOAD_INVALID", "Stream uploads after first write are not allowed")
		return false
	}
	if cr := r.Header.Get("Content-Range"); cr != "" {
		first, _, _ := strings.Cut(cr, "-")
		if n, err := strconv.Atoi(first); err != nil || n != len(up.data) {
			w.Header().Set("Range", fmt.Sprintf("0-%d", max(len(up.data)-1, 0)))
			writeErr(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "range not satisfiable")
			return false
		}
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", err.Error())
		return false
	}
	up.data = append(up.data, b...)
	return true
}

// limitChunk does to a PATCH what GHCR does with chunkLimit (docs/registries/ghcr.md, Upload
// sessions): one with Content-Range of more than chunkLimit bytes is 416, and one without
// Content-Range throws away what the session held. The caller holds f.mu.
func (f *fakeRegistry) limitChunk(w http.ResponseWriter, r *http.Request, up *fakeUpload) bool {
	switch {
	case f.chunkLimit == 0:
	case r.Header.Get("Content-Range") == "":
		up.data = nil
	case r.ContentLength > int64(f.chunkLimit):
		writeErr(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "the request body is too large")
		return false
	}
	return true
}

// answerSession answers status with the session's Location, Range and, when stateful, its
// Docker-Upload-UUID. The caller holds f.mu.
func (f *fakeRegistry) answerSession(w http.ResponseWriter, id string, up *fakeUpload, status int) {
	loc := "/v2/" + up.repo + "/blobs/uploads/" + id
	if f.stateful {
		loc = f.srv.URL + loc + "?_state=" + strconv.Itoa(up.state)
		w.Header().Set("Docker-Upload-UUID", "uuid-"+id)
	}
	w.Header().Set("Location", loc)
	w.Header().Set("Range", fmt.Sprintf("0-%d", max(len(up.data)-1, 0)))
	w.WriteHeader(status)
}

// openUploads returns how many sessions are open.
func (f *fakeRegistry) openUploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

func (f *fakeRegistry) serveBlob(w http.ResponseWriter, r *http.Request, repo string, d digest.Digest) {
	if r.Method == http.MethodDelete {
		f.deleteBlob(w, repo, d)
		return
	}
	if f.redirect != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		w.Header().Set("Location", f.redirect+"/"+d.String())
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	f.mu.Lock()
	content, ok := f.blobs[repo][d]
	stream := f.streams[d]
	f.mu.Unlock()
	switch {
	case stream != nil:
		stream(w)
	case !ok:
		writeErr(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
	default:
		// ServeContent: Content-Length, Accept-Ranges, HEAD, a Range with 206, 416.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", d.String())
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
	}
}

func (f *fakeRegistry) deleteBlob(w http.ResponseWriter, repo string, d digest.Digest) {
	if f.deleteDisabled {
		writeErr(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "The operation is unsupported.")
		return
	}
	f.mu.Lock()
	_, ok := f.blobs[repo][d]
	delete(f.blobs[repo], d)
	f.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (f *fakeRegistry) serveReferrers(w http.ResponseWriter, repo string, d digest.Digest) {
	if !f.referrers {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	f.mu.Lock()
	idx := ocispec.Index{Manifests: []ocispec.Descriptor{}}
	idx.SchemaVersion, idx.MediaType = 2, ocispec.MediaTypeImageIndex
	for ref, m := range f.manifests[repo] {
		if m.subject == d && ref == m.desc.Digest.String() {
			idx.Manifests = append(idx.Manifests, m.desc)
		}
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", ocispec.MediaTypeImageIndex)
	_ = json.NewEncoder(w).Encode(idx)
}

func (f *fakeRegistry) serveTags(w http.ResponseWriter, repo string) {
	f.mu.Lock()
	ms, ok := f.manifests[repo]
	var tags []string
	for ref := range ms {
		if _, err := digest.Parse(ref); err != nil {
			tags = append(tags, ref)
		}
	}
	f.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "NAME_UNKNOWN", "repository name not known to registry")
		return
	}
	slices.Sort(tags)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
}

// writeErr answers status with one error of the distribution spec.
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

// syncBuffer is a bytes.Buffer for a slog handler that tests read while requests log.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testLogger returns a debug logger that writes text lines to the returned buffer.
func testLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}
