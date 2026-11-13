package probe

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// monoWrites is how many writes the monotonic-reads measurement makes, and how many deletes.
const monoWrites = 3

// monoState is what a read of the monotonic-reads measurement found: the state after the
// change, the one before it, or neither (an error, another digest).
type monoState int

const (
	monoOther monoState = iota
	monoBefore
	monoAfter
)

// monoRead is a read of the monotonic-reads measurement: a HEAD of target, and what an answer
// to it found, in words for the cell when it is not the state after the change.
type monoRead struct {
	target   string
	manifest bool // a manifest: asked with manifestAccept
	state    func(answer) (monoState, string)
}

// monoChange is a change the measurement makes, and the reads that follow it.
type monoChange struct {
	what  string // "write 2", "the tag move", "delete 1"
	after string // the state after it, in words: "seen", "the new digest", "gone"
	reads map[string]monoRead
}

// monoSeen is what the monotonic-reads measurement found.
type monoSeen struct {
	writes, moves, deletes int
	mu                     sync.Mutex      // guards addrs: a dial may end after its request
	addrs                  map[string]bool // the addresses the connections reached
	back                   []string        // each regression, in order: "write 2, manifest by tag: seen, then 404"
	backs                  map[string]int  // how many reads each regression was
	other                  map[string]int  // the answers of neither state, by words
	unseen                 []string        // "write 2, manifest by tag": not seen within lagLimit
	moveRefused            string
	deleteRefused          string
}

func newMonoSeen() *monoSeen {
	return &monoSeen{addrs: map[string]bool{}, backs: map[string]int{}, other: map[string]int{}}
}

// measureMonotonic measures "Monotonic reads": monoWrites writes in the repository mono, a new
// blob and then a manifest with it as its layer under a new tag; the first tag moved to another
// manifest; then the deletes of the manifests the tags name. After each change, each read goes
// over a new connection, its host name looked up anew (client.fresh), until it sees the change
// and then Options.MonotonicReads times more: a later read that sees the state before the change
// is a regression, as a client behind an endpoint that picks a replica per connection meets one
// (ACR's global endpoint with geo-replication). A move or a delete the registry refuses is not
// measured, and a read that does not see a change within lagLimit ends the measurement.
func (r *run) measureMonotonic(ctx context.Context) error {
	const repo = "mono"
	if err := r.ensureBase(ctx, repo); err != nil {
		return err
	}
	s := newMonoSeen()
	if err := r.monoChanges(ctx, s, repo); err != nil {
		return err
	}
	r.found.notMonotonic = len(s.back) > 0
	r.set("monotonic", monoVerdict(s, r.opts.MonotonicReads))
	return nil
}

// monoChanges makes the changes of measureMonotonic in repo and follows each.
func (r *run) monoChanges(ctx context.Context, s *monoSeen, repo string) error {
	var tagged []lagWrite
	for i := range monoWrites {
		w, err := r.monoWrite(ctx, s, repo, i)
		if err != nil || len(s.unseen) > 0 {
			return err
		}
		tagged = append(tagged, w)
	}
	moved, err := r.monoMove(ctx, s, repo, tagged[0])
	if err != nil || len(s.unseen) > 0 {
		return err
	}
	if moved != nil {
		tagged[0] = *moved
	}
	for i, w := range tagged {
		a, err := r.send(ctx, http.MethodDelete, r.path(repo, "manifests/"+w.digest.String()))
		if err != nil {
			return err
		}
		if !a.ok(http.StatusAccepted, http.StatusOK) {
			s.deleteRefused = a.refusal()
			return nil
		}
		s.deletes++
		if err := r.follow(ctx, s, monoChange{what: fmt.Sprintf("delete %d", i+1), after: "gone", reads: map[string]monoRead{
			readManifest: {target: r.path(repo, "manifests/"+w.digest.String()), manifest: true, state: absent},
			readTag:      {target: r.path(repo, "manifests/"+w.tag), manifest: true, state: absent},
		}}); err != nil || len(s.unseen) > 0 {
			return err
		}
	}
	return nil
}

// monoWrite is the write i of measureMonotonic: a new blob, followed, then a manifest with it as
// its layer under the tag mono-<i>, followed.
func (r *run) monoWrite(ctx context.Context, s *monoSeen, repo string, i int) (lagWrite, error) {
	what := fmt.Sprintf("write %d", i+1)
	data := []byte(fmt.Sprintf("lajstrom registry probe %s: mono %d", r.id, i))
	d, a, err := r.pushBlob(ctx, repo, data)
	if err != nil {
		return lagWrite{}, err
	}
	if !a.ok(http.StatusCreated) {
		return lagWrite{}, fmt.Errorf("push of a blob to %s: %s", r.name(repo), a.refusal())
	}
	if err := r.follow(ctx, s, monoChange{what: what, after: "seen", reads: map[string]monoRead{
		readBlob: {target: r.path(repo, "blobs/"+d.String()), state: blobThere},
	}}); err != nil || len(s.unseen) > 0 {
		return lagWrite{}, err
	}
	m := r.layerManifest(fmt.Sprintf("mono %d", i), ocispec.Descriptor{MediaType: "application/octet-stream", Digest: d, Size: int64(len(data))})
	tag := fmt.Sprintf("mono-%d", i)
	if a, err = r.pushManifest(ctx, repo, tag, m); err != nil {
		return lagWrite{}, err
	}
	if !a.ok(http.StatusCreated, http.StatusOK) {
		return lagWrite{}, fmt.Errorf("push of a manifest to %s:%s: %s", r.name(repo), tag, a.refusal())
	}
	s.writes++
	return lagWrite{tag: tag, digest: m.digest}, r.follow(ctx, s, monoChange{what: what, after: "seen", reads: map[string]monoRead{
		readManifest: {target: r.path(repo, "manifests/"+m.digest.String()), manifest: true, state: there},
		readTag:      {target: r.path(repo, "manifests/"+tag), manifest: true, state: tagAt(m.digest, "")},
	}})
}

// monoMove pushes another manifest under w's tag and follows the tag: the move Lajstrom makes for
// an oci proxy's upstream tag (ADR 0018) and for a sha256-<hex> index of the tag schema. It
// returns the manifest the tag names now, nil when the registry refused the move.
func (r *run) monoMove(ctx context.Context, s *monoSeen, repo string, w lagWrite) (*lagWrite, error) {
	m := r.layerManifest("mono moved", emptyDesc)
	a, err := r.pushManifest(ctx, repo, w.tag, m)
	if err != nil {
		return nil, err
	}
	if !a.ok(http.StatusCreated, http.StatusOK) {
		s.moveRefused = a.refusal()
		return nil, nil
	}
	s.moves++
	return &lagWrite{tag: w.tag, digest: m.digest}, r.follow(ctx, s, monoChange{what: "the tag move", after: "the new digest", reads: map[string]monoRead{
		readTag: {target: r.path(repo, "manifests/"+w.tag), manifest: true, state: tagAt(m.digest, w.digest)},
	}})
}

// there is the state of a manifest after its write: 200, or 404 before it.
func there(a answer) (monoState, string) {
	switch a.status {
	case http.StatusOK:
		return monoAfter, ""
	case http.StatusNotFound:
		return monoBefore, "404"
	}
	return monoOther, a.refusal()
}

// blobThere is the state of a blob after its write: 200 or a redirect (a blob on S3), or 404.
func blobThere(a answer) (monoState, string) {
	if a.status/100 == 3 && a.location() != nil {
		return monoAfter, ""
	}
	return there(a)
}

// absent is the state of a manifest after its delete: 404, or 200 before it.
func absent(a answer) (monoState, string) {
	switch a.status {
	case http.StatusNotFound:
		return monoAfter, ""
	case http.StatusOK:
		return monoBefore, "200"
	}
	return monoOther, a.refusal()
}

// tagAt is the state of a tag pushed or moved to d: 200 with d; before it 404, or 200 with old,
// the digest the tag named before a move.
func tagAt(d, old digest.Digest) func(answer) (monoState, string) {
	return func(a answer) (monoState, string) {
		got := a.header.Get("Docker-Content-Digest")
		switch {
		case a.status == http.StatusOK && got == d.String():
			return monoAfter, ""
		case a.status == http.StatusNotFound:
			return monoBefore, "404"
		case a.status == http.StatusOK && old != "" && got == old.String():
			return monoBefore, "the old digest"
		case a.status == http.StatusOK:
			return monoOther, "200 with another digest"
		}
		return monoOther, a.refusal()
	}
}

// follow asks each read of c, each time over a new connection, until it sees the change, and
// then Options.MonotonicReads times more, recording into s each later read that does not see it.
// Until every read has seen it, the reads are asked again as watch does, at most for lagLimit; a
// read that has not seen it by then is unseen.
func (r *run) follow(ctx context.Context, s *monoSeen, c monoChange) error {
	at, wait := time.Now(), lagFirst
	left := map[string]int{} // reads still to make after the first that saw the change
	for {
		waiting, err := r.followRound(ctx, s, c, left)
		if err != nil {
			return err
		}
		switch {
		case !waiting && len(left) == len(c.reads) && allZero(left):
			return nil
		case !waiting:
			continue
		case time.Since(at) >= lagLimit:
			for _, k := range readOrder {
				if _, seen := left[k]; !seen && c.reads[k].state != nil {
					s.unseen = append(s.unseen, c.what+", "+k)
					left[k] = 0
				}
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, lagStep)
	}
}

// followRound asks once each read of c that has reads still to make, and reports whether one of
// them has not seen the change yet.
func (r *run) followRound(ctx context.Context, s *monoSeen, c monoChange, left map[string]int) (waiting bool, err error) {
	for _, k := range readOrder {
		read, ok := c.reads[k]
		n, seen := left[k]
		if !ok || seen && n == 0 {
			continue
		}
		a, err := r.freshRead(ctx, s, read)
		if err != nil {
			return false, err
		}
		state, words := read.state(a)
		switch {
		case !seen && state == monoAfter:
			left[k] = r.opts.MonotonicReads
		case !seen:
			waiting = true
		default:
			left[k] = n - 1
			s.record(c, k, state, words)
		}
	}
	return waiting, nil
}

// record records the read k of c, one after the first that saw the change, that found state:
// a regression when it is the state before the change.
func (s *monoSeen) record(c monoChange, k string, state monoState, words string) {
	switch state {
	case monoAfter:
	case monoBefore:
		back := fmt.Sprintf("%s, %s: %s, then %s", c.what, k, c.after, words)
		if s.backs[back] == 0 {
			s.back = append(s.back, back)
		}
		s.backs[back]++
	case monoOther:
		s.other[words]++
	}
}

// allZero reports whether every count of m is 0.
func allZero(m map[string]int) bool {
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

// freshRead sends read over a client of its own (client.fresh), and records into s the address
// its connection reached.
func (r *run) freshRead(ctx context.Context, s *monoSeen, read monoRead) (answer, error) {
	c := r.c.fresh(func(addr string) {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			addr = host
		}
		s.mu.Lock()
		s.addrs[addr] = true
		s.mu.Unlock()
	})
	defer c.close()
	req := request{method: http.MethodHead, target: read.target}
	if read.manifest {
		req.header = manifestAccept
	}
	return c.do(ctx, req)
}

// monoVerdict is the cell "Monotonic reads", each change read again n times after the first
// read that saw it.
func monoVerdict(s *monoSeen, n int) string {
	var changes []string
	if s.writes > 0 {
		changes = append(changes, countOf(s.writes, "write"))
	}
	if s.moves > 0 {
		changes = append(changes, "a tag move")
	}
	if s.deletes > 0 {
		changes = append(changes, countOf(s.deletes, "delete"))
	}
	s.mu.Lock()
	addrs := len(s.addrs)
	s.mu.Unlock()
	scope := fmt.Sprintf("%s, each read %d times more over new connections after the first that saw it, from %s",
		strings.Join(changes, ", "), n, countOf(addrs, "address"))
	var v string
	if len(s.back) == 0 {
		v = "none: no read saw a change undone; " + scope
	} else {
		parts := make([]string, 0, len(s.back))
		for _, b := range s.back {
			parts = append(parts, fmt.Sprintf("%s (%d of %d)", b, s.backs[b], n))
		}
		v = fmt.Sprintf("**not monotonic**: %s; %s", strings.Join(parts, "; "), scope)
	}
	if len(s.other) > 0 {
		var o []string
		for _, words := range slices.Sorted(maps.Keys(s.other)) {
			o = append(o, fmt.Sprintf("%s (%d)", words, s.other[words]))
		}
		v += "; reads that answered neither: " + strings.Join(o, ", ")
	}
	if s.moveRefused != "" {
		v += "; the tag move not measured: the registry refused it (" + s.moveRefused + ")"
	}
	if s.deleteRefused != "" {
		v += "; deletes not measured: the registry refused a delete by digest (" + s.deleteRefused + ")"
	}
	if len(s.unseen) > 0 {
		v += fmt.Sprintf("; not seen within %s: %s, which ended the measurement", lagLimit, strings.Join(s.unseen, ", "))
	}
	return v
}

// countOf is n things, in words: "1 write", "3 writes".
func countOf(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	if strings.HasSuffix(thing, "s") {
		return fmt.Sprintf("%d %ses", n, thing)
	}
	return fmt.Sprintf("%d %ss", n, thing)
}
