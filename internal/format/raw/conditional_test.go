package raw

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
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

// TestIfModifiedSinceAfterDelete is #1419: the newest version deleted, as a rollback through the
// management API deletes it, makes the version before it the newest again, whose created is
// before the date a client got with the deleted file. That date gets 200 and the file newest
// again, dated the second after the delete, or undated while that second has not come.
func TestIfModifiedSinceAfterDelete(t *testing.T) {
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	plugin := &builtAt{Streamer: Plugin{}, at: at}
	e := newEnv(t, envOptions{plugin: plugin})
	etag := func(content string) string { return `"` + digest.FromString(content).String() + `"` }

	e.put(fileTxt, "first")
	plugin.at = at.Add(time.Second)
	second := e.put(fileTxt, "second")
	held := plugin.at.Format(http.TimeFormat)
	e.revalidate(t, "before the delete", []revalidation{
		{"no condition", "", nil, http.StatusOK, "second", held},
		{"its own date", "", []string{"If-Modified-Since", held}, http.StatusNotModified, "", ""},
	})

	pkg, err := e.st.GetPackage(t.Context(), e.repo.ID, fileTxt)
	must(t, err)
	v, err := e.st.GetVersionByDigest(t.Context(), e.repo.ID, digest.Digest(second.Digest))
	must(t, err)
	if _, err := e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: index.ManifestPath(*pkg, *v), Reference: second.Digest},
		auth.Principal{Kind: auth.KindUser, Name: "alice"}); err != nil {
		t.Fatalf("delete of the second version: %v", err)
	}
	pkg, err = e.st.GetPackage(t.Context(), e.repo.ID, fileTxt)
	must(t, err)
	if pkg.LastDeleteAt == nil {
		t.Fatal("the delete is not recorded on the package")
	}
	next := pkg.LastDeleteAt.Truncate(time.Second).Add(time.Second)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, hdr := range [][]string{{"If-Modified-Since", held}, {"If-None-Match", etag("second"), "If-Modified-Since", held}} {
			start := time.Now()
			rr := e.do(method, target(fileTxt), nil, hdr...)
			end := time.Now()
			lastModified := rr.Header().Get("Last-Modified")
			switch {
			case rr.Code != http.StatusOK:
				t.Errorf("after the delete, %s %v = %d, want 200", method, hdr, rr.Code)
			case method == http.MethodGet && rr.Body.String() != "first":
				t.Errorf("after the delete, GET %v body %q, want %q", hdr, rr.Body, "first")
			case lastModified == next.Format(http.TimeFormat) && !next.After(end):
			case lastModified == "" && next.After(start):
			default:
				t.Errorf("after the delete at %v, %s %v Last-Modified %q, want the second after it from then on, none before",
					pkg.LastDeleteAt, method, hdr, lastModified)
			}
		}
	}
}

// TestLastModifiedAfterDelete: the dates of the newest version with the last delete L (#1419;
// docs/spec/http-routing.md §3, raw), whose second the test chooses as it chooses the versions'.
// The newest created at L or before is dated L + 1 s, which is 304 and the deleted version's date
// 200; a version created in the second after L has no date; one created later has its created.
// While L + 1 s has not come, none; a pinned version keeps its created.
func TestLastModifiedAfterDelete(t *testing.T) {
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	plugin := &builtAt{Streamer: Plugin{}, at: at}
	deleted := &deletedAt{}
	e := newEnv(t, envOptions{plugin: plugin, store: func(st index.Store) index.Store { deleted.Store = st; return deleted }})
	date := func(sec int) string { return at.Add(time.Duration(sec) * time.Second).Format(http.TimeFormat) }

	first := e.put(fileTxt, "first")
	deleted.at = at.Add(5*time.Second + 300*time.Millisecond) // the delete of a version created after first
	pinned := "?digest=" + first.Digest
	e.revalidate(t, "the newest created before the delete", []revalidation{
		{"no condition", "", nil, http.StatusOK, "first", date(6)},
		{"the deleted version's date", "", []string{"If-Modified-Since", date(5)}, http.StatusOK, "first", date(6)},
		{"its own created", "", []string{"If-Modified-Since", date(0)}, http.StatusOK, "first", date(6)},
		{"the second after the delete", "", []string{"If-Modified-Since", date(6)}, http.StatusNotModified, "", ""},
		{"pinned", pinned, nil, http.StatusOK, "first", date(0)},
	})

	plugin.at = at.Add(6 * time.Second)
	e.put(fileTxt, "second")
	e.revalidate(t, "a version created in the second after the delete", []revalidation{
		{"no condition", "", nil, http.StatusOK, "second", ""},
		{"the date the first had", "", []string{"If-Modified-Since", date(6)}, http.StatusOK, "second", ""},
	})

	plugin.at = at.Add(7 * time.Second)
	e.put(fileTxt, "third")
	e.revalidate(t, "a version created two seconds after the delete", []revalidation{
		{"the date the first had", "", []string{"If-Modified-Since", date(6)}, http.StatusOK, "third", date(7)},
		{"its own date", "", []string{"If-Modified-Since", date(7)}, http.StatusNotModified, "", ""},
	})

	deleted.at = time.Now().Add(time.Hour) // a second after the delete that has not come
	e.revalidate(t, "before the second after the delete", []revalidation{
		{"no condition", "", nil, http.StatusOK, "third", ""},
		{"its own date", "", []string{"If-Modified-Since", date(7)}, http.StatusOK, "third", ""},
		{"pinned", pinned, nil, http.StatusOK, "first", date(0)},
	})
}

// deletedAt is the store whose packages were last deleted at at, a time of the test's choosing,
// for the routes, which read it; zero is the store's own.
type deletedAt struct {
	index.Store
	at time.Time
}

func (s *deletedAt) GetPackage(ctx context.Context, repoID int64, name string) (*index.Package, error) {
	p, err := s.Store.GetPackage(ctx, repoID, name)
	if err == nil && !s.at.IsZero() {
		p.LastDeleteAt = &s.at
	}
	return p, err
}

// TestNewestDate: the Last-Modified of the newest version, from the created of the versions, the
// last delete and the time of the answer (#1274, #1419; docs/spec/http-routing.md §3, raw).
func TestNewestDate(t *testing.T) {
	base := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	sec := func(s float64) time.Time { return base.Add(time.Duration(s * float64(time.Second))) }
	later := sec(3600)
	none := time.Time{}
	for _, tt := range []struct {
		name       string
		created    []float64 // in order: the last is the newest
		lastDelete float64   // -1: none known
		now        time.Time
		want       time.Time
	}{
		{"one version", []float64{0}, -1, later, sec(0)},
		{"two versions in one second", []float64{0, 0.5}, -1, later, none},
		{"two versions a second apart", []float64{0, 1}, -1, later, sec(1)},
		{"a delete two seconds before the newest", []float64{0, 10}, 8.9, later, sec(10)},
		{"a delete two seconds before, two versions in one second", []float64{0, 10, 10.5}, 8.9, later, none},
		{"the newest in the second after the delete", []float64{0, 10.7}, 9.4, later, none},
		{"a delete in the newest's second, after it", []float64{0, 10}, 10.5, later, sec(11)},
		{"a delete after the newest", []float64{0, 10}, 20.3, later, sec(21)},
		{"a delete after two versions in one second", []float64{10, 10.5}, 20.3, later, sec(21)},
		{"an answer in the delete's second", []float64{0, 10}, 20.3, sec(20.9), none},
		{"an answer at the second after the delete", []float64{0, 10}, 20.3, sec(21), sec(21)},
	} {
		versions := make([]index.Version, len(tt.created))
		for i, c := range tt.created {
			versions[i] = index.Version{ID: int64(i + 1), CreatedAt: sec(c)}
		}
		var lastDelete *time.Time
		if tt.lastDelete >= 0 {
			l := sec(tt.lastDelete)
			lastDelete = &l
		}
		if got := newestDate(versions, lastDelete, tt.now); !got.Equal(tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
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
