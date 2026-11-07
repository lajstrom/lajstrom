package deb

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// logBuffer is a log the tests read: a text handler at debug level over a buffer.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// logger returns a logger that writes to l at debug level.
func (l *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// versionOf returns the version of the package name whose variant is variant.
func (p *pipeline) versionOf(t *testing.T, name, variant string) index.Version {
	t.Helper()
	pkg, err := p.st.GetPackage(t.Context(), p.repo.ID, name)
	must(t, err)
	vs, err := p.st.ListVersions(t.Context(), pkg.ID)
	must(t, err)
	for _, v := range vs {
		if v.Variant == variant {
			return v
		}
	}
	t.Fatalf("%s has no version %s", name, variant)
	return index.Version{}
}

// TestPool fetches the pool files of fixtureSet by the Filename of Packages: the bytes uploaded,
// with the content type of a .deb, the ETag of the blob, Last-Modified and
// Docker-Content-Digest of the newest version that holds the file (hello 1.0.0-1 is in jammy
// and jammy-updates, one blob), a lib* package of another source under libf/libfoo/, a package
// with an epoch under its file name without it.
func TestPool(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	fixtureSet(t, e.pipeline)
	hello, _ := testutil.Deb(testutil.DebOptions{})
	data, _ := testutil.Deb(testutil.DebOptions{Name: "hello-data", Version: "1:2.0-1", Arch: "all", Depends: "hello (>= 1.0.0)",
		Description: "the greetings of hello\nEvery architecture shares them."})
	libfoo := debOf(t, libfooControl, ptr("b026324c6904b2a9cb4b88d6d61c81d1  usr/lib/libfoo.so.2\n"), "xz")

	newer := e.versionOf(t, "hello", "jammy-updates/main/amd64")
	newer.CreatedAt = newer.CreatedAt.Add(time.Hour)
	must(t, e.st.UpsertVersion(t.Context(), &newer))

	packages := e.get("dists/jammy/main/binary-amd64/Packages").Body.String() + e.get("dists/jammy/universe/binary-amd64/Packages").Body.String()
	for _, c := range []struct {
		path string
		deb  []byte
		v    index.Version // the version whose manifest digest and created the answer has
	}{
		{"pool/main/h/hello/hello_1.0.0-1_amd64.deb", hello, newer},
		{"pool/main/h/hello-data/hello-data_2.0-1_all.deb", data, e.versionOf(t, "hello-data", "jammy/main/all")},
		{"pool/universe/libf/libfoo/libfoo2_2.1-3_amd64.deb", libfoo, e.versionOf(t, "libfoo2", "jammy/universe/amd64")},
	} {
		if !strings.Contains(packages, "Filename: "+c.path+"\n") {
			t.Errorf("no Packages of jammy lists %s", c.path)
		}
		rr := e.get(c.path)
		if !bytes.Equal(rr.Body.Bytes(), c.deb) {
			t.Errorf("GET %s: %d bytes, not the .deb uploaded", c.path, rr.Body.Len())
		}
		want := http.Header{"Content-Type": {packageContentType}, "Etag": {`"` + digest.FromBytes(c.deb).String() + `"`},
			"Content-Length": {strconv.Itoa(len(c.deb))}, "Accept-Ranges": {"bytes"}, "Docker-Content-Digest": {c.v.Digest.String()},
			"Last-Modified": {c.v.CreatedAt.UTC().Format(http.TimeFormat)}}
		for k := range want {
			if got := rr.Header().Get(k); got != want.Get(k) {
				t.Errorf("GET %s: %s %q, want %q", c.path, k, got, want.Get(k))
			}
		}
	}
}

// TestPoolReads checks HEAD, a Range and If-None-Match on a pool file, through ServeBlob.
func TestPoolReads(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	hello, _ := testutil.Deb(testutil.DebOptions{})
	e.upload(t, hello, "jammy")
	const path = "pool/main/h/hello/hello_1.0.0-1_amd64.deb"
	head := e.do(http.MethodHead, path)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(len(hello)) ||
		head.Header().Get("Content-Type") != packageContentType {
		t.Errorf("HEAD = %d, %d bytes, headers %v", head.Code, head.Body.Len(), head.Header())
	}
	rr := e.do(http.MethodGet, path, "Range", "bytes=0-7")
	if rr.Code != http.StatusPartialContent || rr.Body.String() != "!<arch>\n" || rr.Header().Get("Content-Range") != "bytes 0-7/"+strconv.Itoa(len(hello)) {
		t.Errorf("GET with a Range = %d %q, Content-Range %q", rr.Code, rr.Body, rr.Header().Get("Content-Range"))
	}
	etag := `"` + digest.FromBytes(hello).String() + `"`
	reads := len(e.prov.OpenBlobs())
	if rr := e.do(http.MethodGet, path, "If-None-Match", etag); rr.Code != http.StatusNotModified || len(e.prov.OpenBlobs()) != reads {
		t.Errorf("GET with If-None-Match = %d, %d reads of the registry", rr.Code, len(e.prov.OpenBlobs())-reads)
	}
}

// TestPoolNotFound checks the paths that name a file the pool does not hold there: another
// component, another first letter or source than PoolPath of the version's source, a name
// that no version has.
func TestPoolNotFound(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	fixtureSet(t, e.pipeline)
	for _, path := range []string{
		"pool/universe/h/hello/hello_1.0.0-1_amd64.deb",
		"pool/main/x/hello/hello_1.0.0-1_amd64.deb",
		"pool/main/h/hullo/hello_1.0.0-1_amd64.deb",
		"pool/universe/l/libfoo/libfoo2_2.1-3_amd64.deb",
		"pool/universe/libf/libfoo2/libfoo2_2.1-3_amd64.deb",
		"pool/main/h/hello/hello_1:1.0.0-1_amd64.deb",
		"pool/main/h/hello/Packages",
	} {
		if rr := e.do(http.MethodGet, path); rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "apt/ubuntu has no "+path) {
			t.Errorf("GET %s = %d %q, want 404", path, rr.Code, rr.Body)
		}
		if rr := e.do(http.MethodHead, path); rr.Code != http.StatusNotFound {
			t.Errorf("HEAD %s = %d, want 404", path, rr.Code)
		}
	}
	if n := len(e.prov.OpenBlobs()) + len(e.prov.StatBlobs()); n != 0 {
		t.Errorf("%d reads of the registry for files the pool does not hold", n)
	}
}

// TestPoolRedirect checks the 307 of a backend that gives a download URL, through ServeBlob,
// and the stream with policy.blobRedirect never; HEAD never redirects.
func TestPoolRedirect(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	hello, info := testutil.Deb(testutil.DebOptions{})
	e.upload(t, hello, "jammy")
	e.prov.RedirectURL = func(name string, d digest.Digest) (string, bool) {
		return "https://cdn.test/" + name + "/" + d.Encoded(), true
	}
	const path = "pool/main/h/hello/hello_1.0.0-1_amd64.deb"
	rr := e.do(http.MethodGet, path)
	want := "https://cdn.test/apt/ubuntu/hello/" + info.SHA256
	if rr.Code != http.StatusTemporaryRedirect || rr.Header().Get("Location") != want || rr.Header().Get("Docker-Content-Digest") == "" {
		t.Errorf("GET = %d, Location %q; want 307 to %s", rr.Code, rr.Header().Get("Location"), want)
	}
	if rr := e.do(http.MethodHead, path); rr.Code != http.StatusOK || rr.Header().Get("Location") != "" {
		t.Errorf("HEAD = %d, Location %q; want 200", rr.Code, rr.Header().Get("Location"))
	}
	repo := e.repo
	repo.Policy = []byte(`{"blobRedirect": "never"}`)
	must(t, e.st.UpdateRepo(t.Context(), &repo))
	if rr := e.do(http.MethodGet, path); rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), hello) {
		t.Errorf("GET with policy.blobRedirect never = %d with %d bytes; want the stream", rr.Code, rr.Body.Len())
	}
}

// TestPoolTwoContents checks a pool file name that two versions hold with two blobs, which an
// oras push (or two uploads at once) can leave and the upload route refuses: 500 without the
// cause, and the log names both digests and versions for the operator.
func TestPoolTwoContents(t *testing.T) {
	logs := &logBuffer{}
	e := newAptEnv(t, envOptions{log: logs.logger()})
	hello, info := testutil.Deb(testutil.DebOptions{})
	other, otherInfo := testutil.Deb(testutil.DebOptions{Description: "another greeting"})
	e.upload(t, hello, "jammy")
	e.upload(t, other, "jammy-updates") // through the pipeline, which has no pool file name rule
	if info.Filename != otherInfo.Filename || info.SHA256 == otherInfo.SHA256 {
		t.Fatalf("the fixtures are %s %s and %s %s, want one name and two contents", info.Filename, info.SHA256, otherInfo.Filename, otherInfo.SHA256)
	}
	rr := e.do(http.MethodGet, "pool/main/h/hello/"+info.Filename)
	if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "sha256") {
		t.Errorf("GET = %d %q, want 500 without the cause", rr.Code, rr.Body)
	}
	log := logs.String()
	for _, want := range []string{`msg="apt: one pool file name, two contents"`, "path=pool/main/h/hello/" + info.Filename,
		"sha256:" + info.SHA256, "sha256:" + otherInfo.SHA256, `"1.0.0-1 jammy/main/amd64"`, `"1.0.0-1 jammy-updates/main/amd64"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log has no %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "request failed") {
		t.Errorf("the conflict is logged twice:\n%s", log)
	}
}

// TestPoolWithoutMetadata checks a version whose metadata the index left out: its file is
// served on its name alone, whatever the source in the path, and the log says so at debug.
func TestPoolWithoutMetadata(t *testing.T) {
	logs := &logBuffer{}
	e := newAptEnv(t, envOptions{log: logs.logger()})
	hello, _ := testutil.Deb(testutil.DebOptions{})
	e.upload(t, hello, "jammy")
	dropMetadata(t, e.pipeline, "jammy")
	for _, path := range []string{"pool/main/h/hello/hello_1.0.0-1_amd64.deb", "pool/main/x/other/hello_1.0.0-1_amd64.deb"} {
		if rr := e.get(path); !bytes.Equal(rr.Body.Bytes(), hello) {
			t.Errorf("GET %s: not the .deb", path)
		}
	}
	if !strings.Contains(logs.String(), "a pool file without metadata in the index is taken on its name") {
		t.Errorf("no debug line:\n%s", logs)
	}
	if rr := e.do(http.MethodGet, "pool/contrib/h/hello/hello_1.0.0-1_amd64.deb"); rr.Code != http.StatusNotFound {
		t.Errorf("GET in another component = %d, want 404", rr.Code)
	}
}

// TestPoolPathOf checks the source of the pool path: the Source field, in any case, with or
// without a version; the package name without one; nothing known from metadata that is absent,
// not JSON, without control, or with a Source that does not parse.
func TestPoolPathOf(t *testing.T) {
	match := func(md string) *index.FileMatch {
		m := &index.FileMatch{File: index.File{Name: "libfoo2_2.1-3_amd64.deb"}, Package: index.Package{Name: "libfoo2"}}
		if md != "" {
			m.Version.Metadata = []byte(md)
		}
		return m
	}
	for _, tt := range []struct {
		md    string
		want  string
		known bool
	}{
		{`{"control":{"Source":"libfoo (2.1-1)"}}`, "libf/libfoo/libfoo2_2.1-3_amd64.deb", true},
		{`{"control":{"source":"foo"}}`, "f/foo/libfoo2_2.1-3_amd64.deb", true},
		{`{"control":{"Package":"libfoo2"}}`, "libf/libfoo2/libfoo2_2.1-3_amd64.deb", true},
		{"", "", false},
		{`not json`, "", false},
		{`{"filename":"x"}`, "", false},
		{`{"control":{"Source":"Not A Name"}}`, "", false},
	} {
		if got, known := poolPathOf(match(tt.md)); got != tt.want || known != tt.known {
			t.Errorf("poolPathOf(%s) = %q, %v; want %q, %v", tt.md, got, known, tt.want, tt.known)
		}
	}
	m := match(`{"control":{}}`)
	m.Package.Name = ""
	if _, known := poolPathOf(m); known {
		t.Error("poolPathOf of a package without a name is known")
	}
}

// findFailing is a store whose FindFiles fails.
type findFailing struct{ index.Store }

func (findFailing) FindFiles(context.Context, int64, string) ([]index.FileMatch, error) {
	return nil, errBoom
}

// TestPoolFailures checks the 500s of the pool route, with the cause in the log: FindFiles
// fails, the registry of the repo is not configured, the backend does not have the blob (404).
func TestPoolFailures(t *testing.T) {
	logs := &logBuffer{}
	e := newAptEnv(t, envOptions{log: logs.logger(), providers: provider.Map{}})
	hello, _ := testutil.Deb(testutil.DebOptions{})
	e.upload(t, hello, "jammy")
	const path = "pool/main/h/hello/hello_1.0.0-1_amd64.deb"
	if rr := e.do(http.MethodGet, path); rr.Code != http.StatusInternalServerError || !strings.Contains(logs.String(), `registry \"local\" is not configured`) {
		t.Errorf("GET without the registry = %d; log:\n%s", rr.Code, logs)
	}

	h := &handlers{deps: format.Deps{Index: findFailing{e.st}, Log: slog.New(slog.DiscardHandler)}}
	if _, err := h.poolFile(t.Context(), e.repo, "main", "h/hello", "hello_1.0.0-1_amd64.deb"); !errors.Is(err, errBoom) {
		t.Errorf("poolFile with FindFiles failing = %v", err)
	}

	t.Run("blob gone", func(t *testing.T) { // a store of its own: the database is named after the test
		gone := newAptEnv(t, envOptions{})
		gone.upload(t, hello, "jammy")
		v := gone.versionOf(t, "hello", "jammy/main/amd64")
		files, err := gone.st.ListFiles(t.Context(), v.ID)
		must(t, err)
		files[0].Digest = digest.FromString("not in the registry")
		must(t, gone.st.ReplaceFiles(t.Context(), v.ID, files))
		if rr := gone.do(http.MethodGet, path); rr.Code != http.StatusNotFound {
			t.Errorf("GET of a blob the registry does not have = %d, want 404", rr.Code)
		}
	})
}
