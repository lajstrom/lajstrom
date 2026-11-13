package probe

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
)

// replicaRegistry is a registry behind one address whose connections reach one of two
// replicas: the current one, and a stale one that answers as of the change before the last.
// The first lag reads (HEADs) after each change reach the stale one, which is a lag; with
// every, so does each every-th connection from then on, which is not monotonic. refuseMove
// refuses to move a tag (403 DENIED), refuseDelete a delete (405 UNSUPPORTED).
type replicaRegistry struct {
	lag, every   int
	refuseMove   bool
	refuseDelete bool

	mu      sync.Mutex
	conns   int                   // connections opened
	ver     int                   // changes made
	since   int                   // reads since the last change
	history map[string][]revision // "b:<digest>", "m:<digest>", "t:<tag>" → its revisions
	heads   map[int]int           // connection → HEADs it carried
}

// revision is the value of a key from a change on: a digest, "" for absent.
type revision struct {
	ver   int
	value string
}

type connKey struct{}

func serveReplicas(t *testing.T, g *replicaRegistry) *httptest.Server {
	g.history, g.heads = map[string][]revision{}, map[int]int{}
	srv := httptest.NewUnstartedServer(g)
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.conns++
		return context.WithValue(ctx, connKey{}, g.conns)
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// change starts a change: what set records from now on is one change, a request's.
func (g *replicaRegistry) change() {
	g.ver++
	g.since = 0
}

// set records that key holds value from the current change on.
func (g *replicaRegistry) set(key, value string) {
	g.history[key] = append(g.history[key], revision{g.ver, value})
}

// at returns the value of key as of the change v.
func (g *replicaRegistry) at(key string, v int) string {
	value := ""
	for _, rev := range g.history[key] {
		if rev.ver <= v {
			value = rev.value
		}
	}
	return value
}

func (g *replicaRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	path := r.URL.Path
	ref := path[strings.LastIndex(path, "/")+1:]
	switch {
	case r.Method == http.MethodPost:
		w.Header().Set("Location", path+"session")
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut && strings.Contains(path, "/blobs/"):
		g.change()
		g.set("b:"+r.URL.Query().Get("digest"), "x")
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		d := digest.FromBytes(body).String()
		if old := g.at("t:"+ref, g.ver); g.refuseMove && old != "" && old != d {
			fail(w, http.StatusForbidden, "DENIED")
			return
		}
		g.change()
		g.set("m:"+d, d)
		if ref != d {
			g.set("t:"+ref, d)
		}
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodDelete:
		if g.refuseDelete {
			fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED")
			return
		}
		g.change()
		g.set("m:"+ref, "")
		for key, revs := range g.history {
			if strings.HasPrefix(key, "t:") && revs[len(revs)-1].value == ref {
				g.set(key, "")
			}
		}
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodHead:
		g.head(w, r, ref)
	default:
		http.NotFound(w, r)
	}
}

// head answers a HEAD of a blob or a manifest from the replica its connection reaches.
func (g *replicaRegistry) head(w http.ResponseWriter, r *http.Request, ref string) {
	conn := r.Context().Value(connKey{}).(int)
	g.heads[conn]++
	g.since++
	v := g.ver
	if g.since <= g.lag || g.every > 0 && conn%g.every == 0 {
		v--
	}
	key := "t:" + ref
	switch {
	case strings.Contains(r.URL.Path, "/blobs/"):
		key = "b:" + ref
	case strings.HasPrefix(ref, "sha256:"):
		key = "m:" + ref
	}
	value := g.at(key, v)
	if value == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !strings.HasPrefix(key, "b:") {
		w.Header().Set("Docker-Content-Digest", value)
	}
}

// fail answers status with an error document of code.
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"errors":[{"code":"`+code+`"}]}`)
}

// TestMeasureMonotonic runs the monotonic-reads measurement against replicas that agree, that
// lag but never go back, that answer a stale state on some connections, and that refuse a move,
// a delete, or to show a write at all.
func TestMeasureMonotonic(t *testing.T) {
	defer func(l time.Duration) { lagLimit = l }(lagLimit)
	lagLimit = 150 * time.Millisecond
	const none = "none: no read saw a change undone; 3 writes, a tag move, 3 deletes, each read 5 times more over new connections " +
		"after the first that saw it, from 1 address"
	notMonotonic := "no override helps: **reads are not monotonic**; point `url` at one regional endpoint"
	tests := []struct {
		name      string
		g         *replicaRegistry
		want      []string
		overrides string
	}{
		{"one replica", &replicaRegistry{}, []string{none}, "none"},
		{"a lag", &replicaRegistry{lag: 2}, []string{none}, "none"},
		{"a stale replica", &replicaRegistry{every: 3}, []string{
			"**not monotonic**: write 1, blob: seen, then 404 (",
			"write 2, manifest by tag: seen, then 404 (",
			"the tag move, manifest by tag: the new digest, then the old digest (",
			"delete 1, manifest by digest: gone, then 200 (",
			"; 3 writes, a tag move, 3 deletes, each read 5 times more",
		}, notMonotonic},
		{"a move refused", &replicaRegistry{refuseMove: true}, []string{
			"none: no read saw a change undone; 3 writes, 3 deletes, each read",
			"; the tag move not measured: the registry refused it (403 `DENIED`)",
		}, "none"},
		{"deletes refused", &replicaRegistry{refuseDelete: true}, []string{
			"none: no read saw a change undone; 3 writes, a tag move, each read",
			"; deletes not measured: the registry refused a delete by digest (405 `UNSUPPORTED`)",
		}, "none"},
		{"never seen", &replicaRegistry{lag: 1 << 20}, []string{
			"; not seen within 150ms: write 1, blob, which ended the measurement",
		}, "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := serveReplicas(t, tt.g)
			r, err := newRun(config.Registry{Name: "replicas", URL: srv.URL}, Options{Scratch: "lajstrom-probe-mono", MonotonicReads: 5, Log: quiet()})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(context.Background(), tapeKey{}, r.tape)
			if err := r.measureMonotonic(r.in(ctx, "monotonic")); err != nil {
				t.Fatal(err)
			}
			c := r.report.Cell("monotonic")
			for _, want := range tt.want {
				if !c.Measured || !strings.Contains(c.Value, want) {
					t.Errorf("monotonic = %q (measured %v), want …%q…", c.Value, c.Measured, want)
				}
			}
			if got := overridesVerdict(r.report.Capabilities, r.found); got != tt.overrides {
				t.Errorf("overrides = %q, want %q", got, tt.overrides)
			}
			tt.g.mu.Lock()
			defer tt.g.mu.Unlock()
			for conn, n := range tt.g.heads {
				if n > 1 {
					t.Errorf("connection %d carried %d reads, want each read on a connection of its own", conn, n)
				}
			}
		})
	}
}

func TestMonoVerdictCounts(t *testing.T) {
	s := newMonoSeen()
	s.writes, s.addrs["10.0.0.1"], s.addrs["10.0.0.2"] = 1, true, true
	s.back = []string{"write 1, blob: seen, then 404"}
	s.backs[s.back[0]] = 2
	s.other["500"] = 1
	want := "**not monotonic**: write 1, blob: seen, then 404 (2 of 5); 1 write, each read 5 times more over new connections " +
		"after the first that saw it, from 2 addresses; reads that answered neither: 500 (1)"
	if got := monoVerdict(s, 5); got != want {
		t.Errorf("monoVerdict = %q, want %q", got, want)
	}
}
