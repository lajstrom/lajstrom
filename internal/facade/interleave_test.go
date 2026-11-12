package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// The deletes of TestDeleteInterleaved and TestIntegrationDeleteInterleaved race pushes at
// named steps: a call of the registry or of the tag lock, which the test pauses until it lets it
// go. Nothing sleeps: a test waits until a step is reached, or until a call arrives at it.

// step is a point an operation passes: op on the reference ref of the OCI repository name. The
// ops are the registry's resolve, tags, referrers, delete and push (by digest, or by tag with
// the tag as ref), and the tag lock's lock (asked for) and locked (taken, before the work it
// guards).
type step string

func at(op, name, ref string) step { return step(op + " " + name + " " + ref) }

// pauses is what a test pauses at, and the log of the steps reached, in order.
type pauses struct {
	mu       sync.Mutex
	paused   map[step]*pause
	counts   map[step]int
	arrivals []arrival
	log      []step
}

// pause holds the first call that reaches its step until it is released.
type pause struct {
	s                 step
	reached, released chan struct{}
	once              sync.Once
}

// arrival is closed when its step has been reached n times.
type arrival struct {
	s  step
	n  int
	ch chan struct{}
}

func newPauses() *pauses { return &pauses{paused: map[step]*pause{}, counts: map[step]int{}} }

// pauseAt pauses the next call that reaches s.
func (p *pauses) pauseAt(s step) *pause {
	p.mu.Lock()
	defer p.mu.Unlock()
	ps := &pause{s: s, reached: make(chan struct{}), released: make(chan struct{})}
	p.paused[s] = ps
	return ps
}

// arrived returns a channel that is closed once s has been reached n times.
func (p *pauses) arrived(s step, n int) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := make(chan struct{})
	if p.counts[s] >= n {
		close(ch)
		return ch
	}
	p.arrivals = append(p.arrivals, arrival{s: s, n: n, ch: ch})
	return ch
}

// reach is a call at s: it is logged, and waits when s is paused.
func (p *pauses) reach(s step) {
	p.mu.Lock()
	p.log = append(p.log, s)
	p.counts[s]++
	p.arrivals = slices.DeleteFunc(p.arrivals, func(a arrival) bool {
		if p.counts[a.s] < a.n {
			return false
		}
		close(a.ch)
		return true
	})
	ps := p.paused[s]
	delete(p.paused, s)
	p.mu.Unlock()
	if ps != nil {
		close(ps.reached)
		<-ps.released
	}
}

// count returns how many times s was reached.
func (p *pauses) count(s step) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[s]
}

// reset forgets the steps reached so far: the setup's.
func (p *pauses) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts, p.log = map[step]int{}, nil
}

// releaseAll releases every pause, reached or not.
func (p *pauses) releaseAll() {
	p.mu.Lock()
	all := p.paused
	p.paused = map[step]*pause{}
	p.mu.Unlock()
	for _, ps := range all {
		ps.release()
	}
}

// before fails the test unless the first time a was reached comes before the first time b was.
func (p *pauses) before(t *testing.T, a, b step) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	i, j := slices.Index(p.log, a), slices.Index(p.log, b)
	if i < 0 || j < 0 || i > j {
		t.Errorf("want %q before %q; the steps: %q", a, b, p.log)
	}
}

// wait waits up to ten seconds for a call to reach the pause.
func (ps *pause) wait(t *testing.T) {
	t.Helper()
	within(t, ps.reached, fmt.Sprintf("a call at %q", ps.s))
}

func (ps *pause) release() { ps.once.Do(func() { close(ps.released) }) }

// within fails the test when ch is not closed within ten seconds.
func within(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: not within ten seconds", what)
	}
}

// pausedRegistry is a registry whose repositories reach the steps of p; noAPI hides its
// referrers API from Lajstrom.
type pausedRegistry struct {
	provider.Provider
	p     *pauses
	noAPI bool
}

func (g pausedRegistry) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	c, err := g.Provider.Capabilities(ctx)
	if g.noAPI {
		c.Referrers = false
	}
	return c, err
}

func (g pausedRegistry) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := g.Provider.Repository(ctx, name)
	if err != nil {
		return nil, err
	}
	return pausedRepository{Repository: rr, name: name, p: g.p}, nil
}

type pausedRepository struct {
	provider.Repository
	name string
	p    *pauses
}

func (r pausedRepository) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error) {
	r.p.reach(at("resolve", r.name, ref))
	return r.Repository.Resolve(ctx, ref)
}

func (r pausedRepository) Tags(ctx context.Context, last string, fn func([]string) error) error {
	r.p.reach(at("tags", r.name, ""))
	return r.Repository.Tags(ctx, last, fn)
}

func (r pausedRepository) Referrers(ctx context.Context, d ocispec.Descriptor, artifactType string, fn func([]ocispec.Descriptor) error) error {
	r.p.reach(at("referrers", r.name, d.Digest.String()))
	return r.Repository.Referrers(ctx, d, artifactType, fn)
}

func (r pausedRepository) Delete(ctx context.Context, d ocispec.Descriptor) error {
	r.p.reach(at("delete", r.name, d.Digest.String()))
	return r.Repository.Delete(ctx, d)
}

func (r pausedRepository) Push(ctx context.Context, d ocispec.Descriptor, c io.Reader) error {
	r.p.reach(at("push", r.name, d.Digest.String()))
	return r.Repository.Push(ctx, d, c)
}

func (r pausedRepository) PushReference(ctx context.Context, d ocispec.Descriptor, c io.Reader, tag string) error {
	r.p.reach(at("push", r.name, tag))
	return r.Repository.PushReference(ctx, d, c, tag)
}

// pausedStore is the store whose tag locks reach lock, when asked for, and locked, when taken,
// of the OCI repository and the digest of the key.
type pausedStore struct {
	index.Store
	p *pauses
}

func (s pausedStore) WithReferrersLock(ctx context.Context, name string, d digest.Digest, fn func(context.Context) error) error {
	s.p.reach(at("lock", name, d.String()))
	return s.Store.WithReferrersLock(ctx, name, d, func(ctx context.Context) error {
		s.p.reach(at("locked", name, d.String()))
		return fn(ctx)
	})
}

// racer is a facade, h, over the oci repos oci/images and oci/mirror, whose registry calls and
// tag locks reach the steps of p. It has pushed an image D under v1 to x, oci/images/<base>app,
// and a signature of D by digest, R0. Each scenario deletes D from x while something else is
// pushed.
type racer struct {
	t  *testing.T
	h  http.Handler
	st index.Store
	p  *pauses
	racing
	x     string // oci/images/<base>app
	blobs [][]byte
	d     []byte // D's manifest
	dd    digest.Digest
	r0    digest.Digest
}

// racing is what a racer runs on, and how its scenarios delete D.
type racing struct {
	registry string // the registry of the repos
	base     string // the start of every package path
	api      bool   // the registry has the referrers API, as Lajstrom sees it
	schema   bool   // the registry lists referrers in the sha256-<hex> tag schema, which the generic provider writes without the API
	byDigest bool   // the scenarios delete D by its digest, else by its tag v1
}

// newRacer pushes D and R0 through h, after the repo oci/mirror, and forgets the steps that took.
func newRacer(t *testing.T, h http.Handler, st index.Store, p *pauses, o racing) *racer {
	t.Helper()
	// A test that failed leaves no call waiting, before the store it holds a lock of is closed:
	// the cleanups run last first.
	t.Cleanup(p.releaseAll)
	must(t, st.CreateRepo(t.Context(), &index.Repo{Format: "oci", Name: "mirror", Registry: o.registry, Prefix: "oci/mirror", Kind: "hosted"}))
	r := &racer{t: t, h: h, st: st, p: p, racing: o, x: "oci/images/" + o.base + "app"}
	r.blobs = [][]byte{ocispec.DescriptorEmptyJSON.Data, []byte("the layer of D " + o.base), []byte("R0 " + o.base), []byte("R " + o.base)}
	for _, b := range r.blobs {
		wantStatus(t, "the blob "+digest.FromBytes(b).String(), r.send(http.MethodPost, r.x+"/blobs/uploads/?digest="+digest.FromBytes(b).String(),
			b, "Content-Type", octetStream), http.StatusCreated)
	}
	r.d, r.dd = r.image("D")
	wantStatus(t, "PUT D under v1", r.put(r.x, "v1", r.d), http.StatusCreated)
	r0, r0d := r.signature("R0")
	wantStatus(t, "PUT R0", r.put(r.x, r0d.String(), r0), http.StatusCreated)
	r.r0 = r0d
	p.reset()
	return r
}

// image returns an image manifest of the empty config and D's layer, annotated what.
func (r *racer) image(what string) ([]byte, digest.Digest) {
	return manifest(r.t, r.blobs[1], what+" "+r.base)
}

// signature returns a referrer of D whose layer is the blob "<layer> <base>".
func (r *racer) signature(layer string) ([]byte, digest.Digest) {
	return referrerOf(r.t, r.dd, cosignBundle, layer+" "+r.base)
}

// send sends method to /v2/<path>; hdr are header name and value pairs.
func (r *racer) send(method, path string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, "/v2/"+path, rd)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	r.h.ServeHTTP(rr, req)
	return rr
}

func (r *racer) put(name, ref string, body []byte) *httptest.ResponseRecorder {
	return r.send(http.MethodPut, name+"/manifests/"+ref, body, "Content-Type", ocispecManifest)
}

func (r *racer) del(name, ref string) *httptest.ResponseRecorder {
	return r.send(http.MethodDelete, name+"/manifests/"+ref, nil)
}

// ref is the reference the scenarios delete D by: v1, or D's digest.
func (r *racer) ref() string {
	if r.byDigest {
		return r.dd.String()
	}
	return "v1"
}

// deleteD deletes D from x by ref on a goroutine of its own.
func (r *racer) deleteD() <-chan *httptest.ResponseRecorder {
	return async(func() *httptest.ResponseRecorder { return r.del(r.x, r.ref()) })
}

// async runs f on a goroutine of its own and returns its answer's channel.
func async(f func() *httptest.ResponseRecorder) <-chan *httptest.ResponseRecorder {
	ch := make(chan *httptest.ResponseRecorder, 1)
	go func() { ch <- f() }()
	return ch
}

// answered waits up to ten seconds for the answer on ch and checks its status, unless it is 0.
func answered(t *testing.T, what string, ch <-chan *httptest.ResponseRecorder, status int) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case rr := <-ch:
		if status != 0 {
			wantStatus(t, what, rr, status)
		}
		return rr
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no answer within ten seconds", what)
		return nil
	}
}

// pending fails the test when ch has an answer already.
func pending(t *testing.T, what string, ch <-chan *httptest.ResponseRecorder) {
	t.Helper()
	select {
	case rr := <-ch:
		t.Fatalf("%s answered %d, want it to wait: %s", what, rr.Code, rr.Body)
	default:
	}
}

// names checks what ref names in the repository name: the manifest want, or nothing ("").
func (r *racer) names(name, ref string, want digest.Digest) {
	r.t.Helper()
	rr := r.send(http.MethodGet, name+"/manifests/"+ref, nil)
	if want == "" {
		wantError(r.t, "GET "+name+":"+ref, rr, http.StatusNotFound, codeManifestUnknown)
		return
	}
	wantStatus(r.t, "GET "+name+":"+ref, rr, http.StatusOK)
	wantHeader(r.t, "GET "+name+":"+ref, rr, "Docker-Content-Digest", want.String())
}

// exists reports whether ref names a manifest in the repository name.
func (r *racer) exists(name, ref string) bool {
	return r.send(http.MethodHead, name+"/manifests/"+ref, nil).Code == http.StatusOK
}

// referrers checks the referrers the facade lists for D in x.
func (r *racer) referrers(want ...digest.Digest) {
	r.t.Helper()
	rr := r.send(http.MethodGet, r.x+"/referrers/"+r.dd.String(), nil)
	wantStatus(r.t, "referrers of D", rr, http.StatusOK)
	var got []string
	for _, s := range referrersOf(r.t, "referrers of D", rr.Body.Bytes()) {
		got = append(got, strings.Fields(s)[0])
	}
	var w []string
	for _, d := range want {
		w = append(w, d.String())
	}
	slices.Sort(w)
	if !slices.Equal(got, w) {
		r.t.Errorf("referrers of D = %q, want %q", got, w)
	}
}

// deletedReferrers returns the referrers the latest delete of the repo oci/images counted.
func (r *racer) deletedReferrers() float64 {
	r.t.Helper()
	rows, err := r.st.ListAudit(r.t.Context(), index.AuditFilter{Repo: "oci/images", Action: "delete", Limit: 1})
	must(r.t, err)
	if len(rows) != 1 {
		r.t.Fatalf("%d delete rows in the audit log", len(rows))
	}
	var details struct{ Referrers float64 }
	must(r.t, json.Unmarshal(rows[0].Details, &details))
	return details.Referrers
}

// interleaving is one scenario of the deletes: a push into a delete at one of its steps.
type interleaving struct {
	name string
	run  func(t *testing.T, r *racer)
}

// interleavings are the scenarios, the same against the fake and a registry.
func interleavings() []interleaving {
	var all []interleaving
	for _, y := range []struct{ name, path string }{{"another name", "oci/images/%scopy"}, {"another repo", "oci/mirror/%sapp"}} {
		for _, after := range []bool{false, true} {
			when := "while x's DELETE waits"
			if after {
				when = "across x's whole delete"
			}
			all = append(all, interleaving{"1 D into " + y.name + ", " + when, func(t *testing.T, r *racer) {
				raceOtherRepository(t, r, fmt.Sprintf(y.path, r.base), after)
			}})
		}
	}
	all = append(all,
		interleaving{"2a D under v2 before the lock", raceTagBeforeLock},
		interleaving{"2b D under v2 while the delete holds the lock", raceTagUnderLock},
		interleaving{"3 D2 under v3", raceOtherImage},
		interleaving{"3 D2 under v1 before find", raceTagMovedBeforeFind},
		interleaving{"3 D2 under v1 after find, before the lock", func(t *testing.T, r *racer) {
			raceTagMovedAfterFind(t, r, at("lock", r.x, r.dd.String()))
		}},
		interleaving{"3 D2 under v1 under the lock, before the tags are counted", func(t *testing.T, r *racer) {
			raceTagMovedAfterFind(t, r, at("locked", r.x, r.dd.String()))
		}},
		interleaving{"3 D2 under v1 before the backend DELETE", func(t *testing.T, r *racer) {
			raceTagMovedAfterFind(t, r, at("delete", r.x, r.dd.String()))
		}},
		interleaving{"4 D by digest before the backend DELETE", raceDigestBeforeDelete},
		interleaving{"4 D by digest across the delete", raceDigestAcrossDelete},
		interleaving{"5 a referrer before the referrer step", raceReferrerBefore},
		interleaving{"5 a referrer after the referrers are listed", raceReferrerAfterListing},
		interleaving{"5 a referrer while the delete holds the lock", raceReferrerUnderLock},
	)
	return all
}

// TestDeleteInterleaved: the delete of an image D from an oci repo, by tag and by digest,
// against the pushes of D, of another image and of a referrer of D, each at a step of the
// delete; with the referrers API and without. The scenarios say what holds.
func TestDeleteInterleaved(t *testing.T) {
	for _, api := range []bool{true, false} {
		for _, byDigest := range []bool{false, true} {
			for _, s := range interleavings() {
				t.Run(fmt.Sprintf("api=%t/%s/%s", api, deletedBy(byDigest), s.name), func(t *testing.T) {
					p := newPauses()
					e := newEnv(t, envOptions{
						registry: func(f *providertest.Fake) provider.Provider { return pausedRegistry{Provider: f, p: p, noAPI: !api} },
						store:    func(st index.Store) index.Store { return pausedStore{Store: st, p: p} },
					})
					// Names of its own: on SQLite the tag locks are the process's, which a test
					// that failed may still hold.
					s.run(t, newRacer(t, e.h, e.st, p, racing{registry: "local", base: fmt.Sprintf("race-%d/", memoryDBs.Add(1)),
						api: api, byDigest: byDigest}))
				})
			}
		}
	}
}

// deletedBy names the reference of the deletes in a test's name.
func deletedBy(byDigest bool) string {
	if byDigest {
		return "by digest"
	}
	return "by tag"
}

// raceOtherRepository (1): D pushed to y, another OCI repository, with its blobs mounted from
// x, while x's delete waits at its backend DELETE of D, or with y's push held across x's whole
// delete. The tag lock is keyed by the repository too: y's push never waits for x's delete, x's
// delete completes, and y keeps D, which the delete in x leaves alone, as it leaves the blobs
// to the registry's garbage collection.
func raceOtherRepository(t *testing.T, r *racer, y string, across bool) {
	backend := r.p.pauseAt(at("delete", r.x, r.dd.String()))
	var held *pause
	if across {
		held = r.p.pauseAt(at("push", y, "v1"))
	}
	deleted := r.deleteD()
	backend.wait(t)
	for _, b := range r.blobs[:2] {
		d := digest.FromBytes(b)
		answered(t, "mount "+d.String()+" from x", async(func() *httptest.ResponseRecorder {
			return r.send(http.MethodPost, y+"/blobs/uploads/?mount="+d.String()+"&from="+r.x, nil)
		}), http.StatusCreated)
	}
	pushed := async(func() *httptest.ResponseRecorder { return r.put(y, "v1", r.d) })
	if across {
		held.wait(t)
		backend.release()
		answered(t, "DELETE D from x", deleted, http.StatusAccepted)
		held.release()
		answered(t, "PUT y:v1 after x's delete", pushed, http.StatusCreated)
	} else {
		answered(t, "PUT y:v1 while x's DELETE waits", pushed, http.StatusCreated)
		backend.release()
		answered(t, "DELETE D from x", deleted, http.StatusAccepted)
	}
	r.names(r.x, "v1", "")
	r.names(r.x, r.dd.String(), "")
	r.names(y, "v1", r.dd)
	for _, b := range r.blobs[:2] {
		wantStatus(t, "HEAD a blob of D in y", r.send(http.MethodHead, y+"/blobs/"+digest.FromBytes(b).String(), nil), http.StatusOK)
	}
	if n := r.p.count(at("lock", r.x, r.dd.String())); n != 1 {
		t.Errorf("x's tag lock of D asked for %d times, want once, by the delete", n)
	}
}

// raceTagBeforeLock (2a): D pushed under v2 after the delete resolved its reference and before
// it takes the tag lock. The delete then counts v2: by tag v1 it is refused, 409 DENIED naming
// v2, and nothing is deleted; by digest it deletes D with both tags and R0.
func raceTagBeforeLock(t *testing.T, r *racer) {
	lock := r.p.pauseAt(at("lock", r.x, r.dd.String()))
	deleted := r.deleteD()
	lock.wait(t)
	answered(t, "PUT v2 before the delete's lock", async(func() *httptest.ResponseRecorder { return r.put(r.x, "v2", r.d) }), http.StatusCreated)
	lock.release()
	if r.byDigest {
		answered(t, "DELETE by digest", deleted, http.StatusAccepted)
		r.names(r.x, "v1", "")
		r.names(r.x, "v2", "")
		r.names(r.x, r.r0.String(), "")
		r.referrers()
		return
	}
	rr := answered(t, "DELETE D", deleted, 0)
	if msg := wantError(t, "DELETE v1", rr, http.StatusConflict, codeDenied); !strings.Contains(msg, "also tagged v2") {
		t.Errorf("the refusal %q does not name v2", msg)
	}
	r.names(r.x, "v1", r.dd)
	r.names(r.x, "v2", r.dd)
	r.referrers(r.r0)
}

// raceTagUnderLock (2b): D pushed under v2 while the delete holds the tag lock. The push
// asks for the same lock and waits; it pushes after the backend DELETE, and so creates D again
// under v2, without R0: the delete took the referrers, and a push of the subject does not bring
// them back.
func raceTagUnderLock(t *testing.T, r *racer) {
	locked := r.p.pauseAt(at("locked", r.x, r.dd.String()))
	deleted := r.deleteD()
	locked.wait(t)
	pushed := async(func() *httptest.ResponseRecorder { return r.put(r.x, "v2", r.d) })
	within(t, r.p.arrived(at("lock", r.x, r.dd.String()), 2), "the push asks for the delete's tag lock")
	pending(t, "PUT v2 while the delete holds the lock", pushed)
	locked.release()
	answered(t, "DELETE D", deleted, http.StatusAccepted)
	answered(t, "PUT v2 after the delete", pushed, http.StatusCreated)
	r.p.before(t, at("delete", r.x, r.dd.String()), at("push", r.x, "v2"))
	r.names(r.x, "v1", "")
	r.names(r.x, "v2", r.dd)
	r.names(r.x, r.r0.String(), "")
	r.referrers()
}

// raceOtherImage (3): another image, D2, pushed under a new tag v3 while the delete of D holds
// D's tag lock: D2's lock is another, the push does not wait, and the two are independent.
func raceOtherImage(t *testing.T, r *racer) {
	d2, dd2 := r.image("D2")
	locked := r.p.pauseAt(at("locked", r.x, r.dd.String()))
	deleted := r.deleteD()
	locked.wait(t)
	answered(t, "PUT D2 under v3 while the delete holds D's lock", async(func() *httptest.ResponseRecorder { return r.put(r.x, "v3", d2) }),
		http.StatusCreated)
	locked.release()
	answered(t, "DELETE D", deleted, http.StatusAccepted)
	r.names(r.x, "v1", "")
	r.names(r.x, r.dd.String(), "")
	r.names(r.x, "v3", dd2)
}

// raceTagMovedBeforeFind (3): D2 pushed under v1, which moves v1 to it, before the delete
// resolves its reference. A delete by tag deletes what the tag names when the delete resolves
// it: D2, and D stays, untagged, with R0. A delete by digest deletes D, and v1 names D2.
func raceTagMovedBeforeFind(t *testing.T, r *racer) {
	d2, dd2 := r.image("D2")
	find := r.p.pauseAt(at("resolve", r.x, r.ref()))
	deleted := r.deleteD()
	find.wait(t)
	answered(t, "PUT D2 under v1", async(func() *httptest.ResponseRecorder { return r.put(r.x, "v1", d2) }), http.StatusCreated)
	find.release()
	answered(t, "DELETE D", deleted, http.StatusAccepted)
	if r.byDigest {
		r.names(r.x, "v1", dd2)
		r.names(r.x, r.dd.String(), "")
		r.referrers()
		return
	}
	r.names(r.x, "v1", "")
	r.names(r.x, dd2.String(), "")
	r.names(r.x, r.dd.String(), r.dd)
	r.referrers(r.r0)
}

// raceTagMovedAfterFind (3): D2 pushed under v1 once the delete has resolved its reference to
// D, at step s: before the delete takes D's tag lock, under it before it counts the tags, or
// before the backend DELETE of D. The push takes D2's lock and does not wait. The delete deletes
// D by its digest, which takes no tag that names D2: v1 names D2, which is intact.
func raceTagMovedAfterFind(t *testing.T, r *racer, s step) {
	d2, dd2 := r.image("D2")
	ps := r.p.pauseAt(s)
	deleted := r.deleteD()
	ps.wait(t)
	answered(t, "PUT D2 under v1", async(func() *httptest.ResponseRecorder { return r.put(r.x, "v1", d2) }), http.StatusCreated)
	ps.release()
	answered(t, "DELETE D", deleted, http.StatusAccepted)
	r.names(r.x, "v1", dd2)
	r.names(r.x, dd2.String(), dd2)
	r.names(r.x, r.dd.String(), "")
	r.referrers()
}

// raceDigestBeforeDelete (4): D pushed again by its digest while the delete waits at its
// backend DELETE of D. A push by digest takes no tag lock (passThrough): it adds no tag for the
// delete to count. The client gets 201, and D is gone right after: the push is ordered before
// the delete. A lock would not change what a client can see, for the push that ends before the
// delete takes the lock is answered the same.
func raceDigestBeforeDelete(t *testing.T, r *racer) {
	backend := r.p.pauseAt(at("delete", r.x, r.dd.String()))
	deleted := r.deleteD()
	backend.wait(t)
	answered(t, "PUT D by digest while the delete waits", async(func() *httptest.ResponseRecorder { return r.put(r.x, r.dd.String(), r.d) }),
		http.StatusCreated)
	if n := r.p.count(at("lock", r.x, r.dd.String())); n != 1 {
		t.Errorf("D's tag lock asked for %d times, want once, by the delete", n)
	}
	backend.release()
	answered(t, "DELETE D", deleted, http.StatusAccepted)
	r.names(r.x, r.dd.String(), "")
	r.names(r.x, "v1", "")
}

// raceDigestAcrossDelete (4): D pushed again by its digest, held at the registry across the
// whole delete: the delete does not wait for it, and the push, answered 201 after the delete's
// 202, stores D again, untagged and without R0.
func raceDigestAcrossDelete(t *testing.T, r *racer) {
	held := r.p.pauseAt(at("push", r.x, r.dd.String()))
	pushed := async(func() *httptest.ResponseRecorder { return r.put(r.x, r.dd.String(), r.d) })
	held.wait(t)
	answered(t, "DELETE D while the push waits", r.deleteD(), http.StatusAccepted)
	held.release()
	answered(t, "PUT D by digest", pushed, http.StatusCreated)
	r.names(r.x, r.dd.String(), r.dd)
	r.names(r.x, "v1", "")
	r.referrers()
}

// raceReferrerBefore (5): a signature R of D pushed by digest before the delete lists D's
// referrers: with the referrers API at the listing itself, for the push takes no lock; without
// it before the delete takes D's tag lock, which the push holds. The delete takes R with D and
// counts it.
func raceReferrerBefore(t *testing.T, r *racer) {
	s := at("referrers", r.x, r.dd.String())
	if !r.api {
		s = at("lock", r.x, r.dd.String())
	}
	ps := r.p.pauseAt(s)
	deleted := r.deleteD()
	ps.wait(t)
	sig, sigd := r.signature("R")
	answered(t, "PUT R", async(func() *httptest.ResponseRecorder { return r.put(r.x, sigd.String(), sig) }), http.StatusCreated)
	ps.release()
	answered(t, "DELETE D", deleted, http.StatusAccepted)
	r.names(r.x, sigd.String(), "")
	r.names(r.x, r.dd.String(), "")
	r.referrers()
	if n := r.deletedReferrers(); n != 2 {
		t.Errorf("the delete counted %v referrers, want 2: R0 and R", n)
	}
}

// raceReferrerAfterListing (5), with the referrers API: R pushed by digest after the delete
// listed D's referrers and before the backend DELETE of D. The push takes no lock, the delete
// does not see R, and R stays, listed under the digest of a subject that is gone: what a push
// of R after the delete leaves too (raceReferrerUnderLock), and what the OCI distribution spec
// asks a registry to accept.
func raceReferrerAfterListing(t *testing.T, r *racer) {
	if !r.api {
		t.Skip("without the referrers API the push of a referrer takes its subject's tag lock: raceReferrerUnderLock")
	}
	backend := r.p.pauseAt(at("delete", r.x, r.dd.String()))
	deleted := r.deleteD()
	backend.wait(t)
	sig, sigd := r.signature("R")
	answered(t, "PUT R", async(func() *httptest.ResponseRecorder { return r.put(r.x, sigd.String(), sig) }), http.StatusCreated)
	backend.release()
	answered(t, "DELETE D", deleted, http.StatusAccepted)
	r.names(r.x, r.dd.String(), "")
	r.names(r.x, sigd.String(), sigd)
	r.referrers(sigd)
	if n := r.deletedReferrers(); n != 1 {
		t.Errorf("the delete counted %v referrers, want 1: R0", n)
	}
}

// raceReferrerUnderLock (5): R pushed while the delete holds D's tag lock, by tag with the
// referrers API, by digest without it: either way the push takes its subject's tag lock, waits,
// and is stored after the delete, its subject gone. An oci repo takes a referrer whose subject
// does not exist (http-routing.md §2; the distribution spec: a registry MUST accept it), so R
// stays and is listed under D's digest; without the API the generic provider writes the
// sha256-<hex> index of D for it, as the spec's procedure says, and the delete of R deletes the
// index again.
func raceReferrerUnderLock(t *testing.T, r *racer) {
	locked := r.p.pauseAt(at("locked", r.x, r.dd.String()))
	deleted := r.deleteD()
	locked.wait(t)
	sig, sigd := r.signature("R")
	ref := sigd.String()
	if r.api {
		ref = "sig"
	}
	pushed := async(func() *httptest.ResponseRecorder { return r.put(r.x, ref, sig) })
	within(t, r.p.arrived(at("lock", r.x, r.dd.String()), 2), "the push of R asks for its subject's tag lock")
	pending(t, "PUT R while the delete holds the lock", pushed)
	locked.release()
	answered(t, "DELETE D", deleted, http.StatusAccepted)
	answered(t, "PUT R after the delete", pushed, http.StatusCreated)
	r.p.before(t, at("delete", r.x, r.dd.String()), at("push", r.x, ref))
	r.names(r.x, r.dd.String(), "")
	r.names(r.x, sigd.String(), sigd)
	r.referrers(sigd)
	schema := "sha256-" + r.dd.Encoded()
	if got := r.exists(r.x, schema); got != r.schema {
		t.Errorf("after the delete, %s names a manifest: %t, want %t", schema, got, r.schema)
	}
	answered(t, "DELETE R", async(func() *httptest.ResponseRecorder { return r.del(r.x, sigd.String()) }), http.StatusAccepted)
	r.names(r.x, schema, "")
	r.referrers()
}
