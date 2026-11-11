package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
)

// routeRegistry keeps blobs and manifests in memory and routes by the end of the path. With
// rnd set it misroutes as Harbor v2.15.4 does (goharbor/harbor#24363): the HEAD of a blob in a
// name with a component manifests or referrers is 404, and in one with tags/list 400, at
// random; GET always finds it. With refuse set, an upload session in such a name is 400
// NAME_INVALID; with lost set, every blob HEAD is 404.
type routeRegistry struct {
	rnd    *rand.Rand
	refuse bool
	lost   bool

	mu      sync.Mutex
	content map[string]bool // name@digest of every blob and manifest
}

func newRouteRegistry(t *testing.T, g *routeRegistry) *httptest.Server {
	g.content = map[string]bool{}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return srv
}

// wordStatus is what a misrouting registry answers the HEAD of a blob in name: 404 or 400,
// 0 for a name without a routing word.
func wordStatus(name string) int {
	segs := strings.Split(name, "/")
	if slices.Contains(segs, "manifests") || slices.Contains(segs, "referrers") {
		return http.StatusNotFound
	}
	for i := range len(segs) - 1 {
		if segs[i] == "tags" && segs[i+1] == "list" {
			return http.StatusBadRequest
		}
	}
	return 0
}

func (g *routeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/"), "/")
	n := len(segs)
	switch {
	case r.URL.Path == "/v2/":
		_, _ = io.WriteString(w, "{}")
	case r.URL.Path == "/v2/_catalog":
		_, _ = io.WriteString(w, `{"repositories":[]}`)
	case n >= 4 && segs[n-3] == "blobs" && segs[n-2] == "uploads":
		g.upload(w, r, strings.Join(segs[:n-3], "/"))
	case n >= 3 && segs[n-2] == "blobs":
		g.blob(w, r, strings.Join(segs[:n-2], "/"), segs[n-1])
	case n >= 3 && segs[n-2] == "manifests":
		g.manifest(w, r, strings.Join(segs[:n-2], "/"), segs[n-1])
	default:
		http.NotFound(w, r)
	}
}

// upload answers the POST of a session and its closing PUT.
func (g *routeRegistry) upload(w http.ResponseWriter, r *http.Request, name string) {
	switch {
	case g.refuse && wordStatus(name) != 0:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":[{"code":"NAME_INVALID","message":"invalid repository name"}]}`)
	case r.Method == http.MethodPost:
		w.Header().Set("Location", r.URL.Path+"1")
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		g.content[name+"@"+digest.FromBytes(body).String()] = true
		w.WriteHeader(http.StatusCreated)
	}
}

// blob answers the HEAD, GET and DELETE of a blob.
func (g *routeRegistry) blob(w http.ResponseWriter, r *http.Request, name, d string) {
	key := name + "@" + d
	switch {
	case !g.content[key]:
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodDelete:
		delete(g.content, key)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodHead && g.lost:
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodHead && g.rnd != nil && wordStatus(name) != 0 && g.rnd.IntN(2) == 0:
		w.WriteHeader(wordStatus(name))
	default:
		_, _ = io.WriteString(w, "blob")
	}
}

// manifest answers the PUT and the DELETE of a manifest.
func (g *routeRegistry) manifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		g.content[name+"@"+digest.FromBytes(body).String()] = true
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		delete(g.content, name+"@"+ref)
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestMeasureRoutingWords(t *testing.T) {
	tests := []struct {
		name   string
		g      *routeRegistry
		result string
		cell   string
	}{
		{"routes right", &routeRegistry{}, RoutingOK, "ok: 20 blob `HEAD`s in each name, all 200; `GET` 200"},
		{"misroutes as Harbor", &routeRegistry{rnd: rand.New(rand.NewPCG(24363, 1))}, //nolint:gosec // a seeded fake, not a secret
			RoutingMisroutes, "**misroutes**: blob `HEAD` in `x/manifests/y` "},
		{"refuses the words", &routeRegistry{refuse: true}, RoutingRefused,
			"**refused**: `x/manifests/y` 400 `NAME_INVALID` to the push of the empty blob, `x/manifests` 400 `NAME_INVALID` to the push of the empty blob, " +
				"`x/referrers/y` 400 `NAME_INVALID` to the push of the empty blob, `x/tags/list` 400 `NAME_INVALID` to the push of the empty blob; " +
				"the others: 20 blob `HEAD`s each, all 200"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newRouteRegistry(t, tt.g)
			r, err := newRun(config.Registry{Name: "r", URL: srv.URL}, Options{Scratch: "lajstrom-probe-route", RoutingHeads: 20, Log: quiet()})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(context.Background(), tapeKey{}, r.tape)
			if err := r.measureRoutingWords(r.in(ctx, "routingWords")); err != nil {
				t.Fatal(err)
			}
			if c := r.report.Cell("routingWords"); !c.Measured || !strings.HasPrefix(c.Value, tt.cell) {
				t.Errorf("routingWords = %q (measured %v), want %q…", c.Value, c.Measured, tt.cell)
			}
			w := r.report.RoutingWords
			if w.Result != tt.result || w.Heads != 20 || len(w.Names) != len(routingNames) {
				t.Fatalf("RoutingWords = %+v", w)
			}
			if c := w.Names[0]; !c.Control || c.Name != "lajstrom-probe-route/route/x" || c.Head[200] != 20 || c.Get != 200 {
				t.Errorf("the plain name = %+v", c)
			}
			checkWordNames(t, tt.result, w.Names[1:])
			r.cleanup(r.in(ctx, "deleteRepository"))
			if len(tt.g.content) > 0 || len(r.report.LeftBehind) > 0 {
				t.Errorf("left %v; the report says %v", tt.g.content, r.report.LeftBehind)
			}
		})
	}
}

// checkWordNames checks the names with a word of a measurement whose result is result: each
// misrouted some HEADs, not all, with its registry's status, or each found its blob every
// time, or each was refused.
func checkWordNames(t *testing.T, result string, names []RoutingName) {
	t.Helper()
	for _, n := range names {
		misses := n.misses()
		switch {
		case result == RoutingMisroutes && (misses == 0 || misses == 20 || n.Head[wordStatus(n.Name)] != misses || n.Get != 200):
			t.Errorf("%s: %v, GET %d; want some HEADs %d, GET 200", n.Name, n.Head, n.Get, wordStatus(n.Name))
		case result == RoutingOK && (n.Head[200] != 20 || n.Get != 200):
			t.Errorf("%s: %v, GET %d", n.Name, n.Head, n.Get)
		case result == RoutingRefused && (n.Refused == "" || n.Head != nil):
			t.Errorf("%s: refused %q, %v", n.Name, n.Refused, n.Head)
		}
	}
}

func TestMeasureRoutingWordsFailsOnThePlainName(t *testing.T) {
	srv := newRouteRegistry(t, &routeRegistry{lost: true})
	r, err := newRun(config.Registry{Name: "r", URL: srv.URL}, Options{Scratch: "s", RoutingHeads: 3, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), tapeKey{}, r.tape)
	err = r.measureRoutingWords(ctx)
	if err == nil || err.Error() != "the plain name s/route/x: blob `HEAD` 3×404, `GET` 200" {
		t.Errorf("measureRoutingWords = %v", err)
	}
	if r.report.RoutingWords != nil || r.report.Cell("routingWords").Measured {
		t.Error("a failed measurement has a result")
	}
}

func TestRoutingVerdict(t *testing.T) {
	const trim = "p/route/"
	plain := RoutingName{Name: trim + "x", Control: true, Head: map[int]int{200: 20}, Get: 200}
	named := func(name string, get int, head map[int]int) RoutingName {
		return RoutingName{Name: trim + name, Head: head, Get: get}
	}
	refused := RoutingName{Name: trim + "x/tags/list", Refused: "400 `NAME_INVALID` to the push of the empty blob"}
	tests := []struct {
		name   string
		names  []RoutingName
		result string
		want   string
	}{
		{"zot", []RoutingName{plain, named("x/manifests", 200, map[int]int{200: 20})}, RoutingOK,
			"ok: 20 blob `HEAD`s in each name, all 200; `GET` 200"},
		{"on S3", []RoutingName{plain, named("x/manifests", 307, map[int]int{307: 20})}, RoutingOK,
			"ok: 20 blob `HEAD`s in each name, all 200 or 307; `GET` 200 or 307"},
		{"harbor v2.15.4", []RoutingName{plain,
			named("x/manifests/y", 200, map[int]int{200: 4, 404: 16}), named("x/manifests", 200, map[int]int{200: 7, 404: 13}),
			named("x/referrers/y", 200, map[int]int{200: 19, 404: 1}), named("x/tags/list", 200, map[int]int{200: 5, 400: 15})},
			RoutingMisroutes,
			"**misroutes**: blob `HEAD` in `x/manifests/y` 16×404, `x/manifests` 13×404, `x/referrers/y` 1×404, `x/tags/list` 15×400 of 20; `GET` 200"},
		{"the GET misses", []RoutingName{plain, named("x/referrers/y", 404, map[int]int{200: 20})}, RoutingMisroutes,
			"**misroutes**: blob `GET` in `x/referrers/y` 404"},
		{"misroutes and refuses", []RoutingName{plain, named("x/manifests", 200, map[int]int{200: 18, 404: 2}), refused}, RoutingMisroutes,
			"**misroutes**: blob `HEAD` in `x/manifests` 2×404 of 20; `GET` 200; refused: `x/tags/list` 400 `NAME_INVALID` to the push of the empty blob"},
		{"refuses", []RoutingName{plain, refused, named("x/manifests", 200, map[int]int{200: 20})}, RoutingRefused,
			"**refused**: `x/tags/list` 400 `NAME_INVALID` to the push of the empty blob; the others: 20 blob `HEAD`s each, all 200"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &RoutingWords{Result: routingResult(tt.names), Heads: 20, Names: tt.names}
			if w.Result != tt.result {
				t.Errorf("routingResult = %s, want %s", w.Result, tt.result)
			}
			if got := routingVerdict(w, trim); got != tt.want {
				t.Errorf("routingVerdict = %q\nwant             %q", got, tt.want)
			}
		})
	}
}

func TestRoutingWordsInTheReport(t *testing.T) {
	r := testReport("harbor")
	r.RoutingWords = &RoutingWords{Result: RoutingMisroutes, Heads: 20, Names: []RoutingName{
		{Name: r.Scratch + "route/x", Control: true, Head: map[int]int{200: 20}, Get: 200},
		{Name: r.Scratch + "route/x/tags/list", Head: map[int]int{200: 5, 400: 15}, Get: 200},
		{Name: r.Scratch + "route/x/manifests", Refused: "400 `NAME_INVALID` to the push of the empty blob"},
	}}
	var b bytes.Buffer
	if err := WriteMarkdown(&b, []*Report{r}, false); err != nil {
		t.Fatal(err)
	}
	want := "Routing words, 20 blob `HEAD`s and a `GET` in each name below `lajstrom-probe-0/route/`: `x` `HEAD` 20×200, `GET` 200; " +
		"`x/tags/list` `HEAD` 5×200 15×400, `GET` 200; `x/manifests` refused: 400 `NAME_INVALID` to the push of the empty blob.\n"
	if !strings.Contains(b.String(), want) {
		t.Errorf("markdown lacks %q:\n%s", want, b.String())
	}
	b.Reset()
	if err := WriteJSON(&b, []*Report{r}, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"result": "misroutes"`, `"name": "lajstrom-probe-0/route/x/tags/list"`, `"400": 15`, `"control": true`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("json lacks %s:\n%s", want, b.String())
		}
	}
	var got []Report
	if err := json.Unmarshal(b.Bytes(), &got); err != nil || got[0].RoutingWords.Names[1].Head[400] != 15 {
		t.Errorf("json reads back as %+v, %v", got, err)
	}
}
