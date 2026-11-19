package raw

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TestIfModifiedSinceSameSecond is #1274: a client that revalidates by date alone (wget -N,
// curl -z) never gets 304 for a version it does not hold. The versions are built at a second of
// the test's choosing. A version alone in its second has its created as Last-Modified, and its
// own date is 304. While a second version shares that second, the newest has no Last-Modified:
// the first's date is 200 with the second's bytes, If-None-Match with the second's ETag (the
// blob digest) is 304 and with the first's 200, whatever If-Modified-Since says; ?digest= names
// one version and keeps its date. A third version a second later is alone in its second again.
func TestIfModifiedSinceSameSecond(t *testing.T) {
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	plugin := &builtAt{Streamer: Plugin{}, at: at}
	e := newEnv(t, envOptions{plugin: plugin})
	date, next := at.Format(http.TimeFormat), at.Add(time.Second).Format(http.TimeFormat)
	etag := func(content string) string { return `"` + digest.FromString(content).String() + `"` }

	first := e.put(fileTxt, "first")
	e.revalidate(t, "one version", []revalidation{
		{"no condition", "", nil, http.StatusOK, "first", date},
		{"its own date", "", []string{"If-Modified-Since", date}, http.StatusNotModified, "", ""},
	})

	e.put(fileTxt, "second")
	pinned := "?digest=" + first.Digest
	e.revalidate(t, "two versions in one second", []revalidation{
		{"no condition", "", nil, http.StatusOK, "second", ""},
		{"the first's date", "", []string{"If-Modified-Since", date}, http.StatusOK, "second", ""},
		{"the second's ETag", "", []string{"If-None-Match", etag("second")}, http.StatusNotModified, "", ""},
		{"the second's ETag and an old date", "", []string{"If-None-Match", etag("second"), "If-Modified-Since", "Sat, 01 Jan 2000 00:00:00 GMT"},
			http.StatusNotModified, "", ""},
		{"the first's ETag and date", "", []string{"If-None-Match", etag("first"), "If-Modified-Since", date}, http.StatusOK, "second", ""},
		{"the first pinned", pinned, nil, http.StatusOK, "first", date},
		{"the first pinned, its date", pinned, []string{"If-Modified-Since", date}, http.StatusNotModified, "", ""},
	})

	plugin.at = at.Add(time.Second)
	e.put(fileTxt, "third")
	e.revalidate(t, "a third version a second later", []revalidation{
		{"its own date", "", []string{"If-Modified-Since", next}, http.StatusNotModified, "", ""},
		{"the shared second's date", "", []string{"If-Modified-Since", date}, http.StatusOK, "third", next},
	})
}

// revalidation is a GET and a HEAD of fileTxt with conditional headers, and what both answer.
type revalidation struct {
	name         string
	query        string   // "?digest=…" or ""
	hdr          []string // header name and value pairs
	status       int
	body         string // of a 200 to the GET
	lastModified string // of a 200; "" is none
}

// revalidate sends each of rs as a GET and as a HEAD and checks the answers.
func (e *env) revalidate(t *testing.T, step string, rs []revalidation) {
	t.Helper()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, r := range rs {
			rr := e.do(method, target(fileTxt)+r.query, nil, r.hdr...)
			lastModified := rr.Header().Get("Last-Modified")
			switch {
			case rr.Code != r.status:
				t.Errorf("%s, %s: %s %v = %d, want %d", step, r.name, method, r.hdr, rr.Code, r.status)
			case r.status != http.StatusOK:
			case method == http.MethodGet && rr.Body.String() != r.body:
				t.Errorf("%s, %s: GET body %q, want %q", step, r.name, rr.Body, r.body)
			case lastModified != r.lastModified:
				t.Errorf("%s, %s: %s Last-Modified %q, want %q", step, r.name, method, lastModified, r.lastModified)
			}
		}
	}
}

// builtAt is the raw plugin whose builds have org.opencontainers.image.created at, a second of
// the test's choosing.
type builtAt struct {
	format.Streamer
	at time.Time
}

func (p *builtAt) Build(ctx context.Context, repo index.Repo, in format.Upload) (*format.Built, error) {
	b, err := p.Streamer.Build(ctx, repo, in)
	if err != nil {
		return nil, err
	}
	b.Manifest.Annotations[ocispec.AnnotationCreated] = p.at.Format(time.RFC3339)
	return b, nil
}
