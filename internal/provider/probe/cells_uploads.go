package probe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// uploadSeen is what the upload measurements found.
type uploadSeen struct {
	first     answer // PATCH abc, Content-Range: 0-2
	status    answer // GET of the session after it
	outOfOrd  answer // PATCH x, Content-Range: 7-7
	afterOdd  answer // GET of the session after the chunk out of order, when it was taken
	cancel    answer // DELETE of another session that holds abc
	afterGet  answer // GET of that session after the cancel
	afterPut  answer // PATCH to it after the cancel
	mount     answer // POST ?mount=&from=
	mounted   answer // HEAD of the blob in the repository it was mounted into
	wrong     answer // PUT ?digest= of other bytes
	wrongGet  answer // GET of that session after it
	wrongHead answer // HEAD of the wrong digest
	empty     answer // the empty blob, POST and PUT
	emptyHead answer
}

// octet is the Content-Type of a chunk.
const octet = "application/octet-stream"

// measureUploads measures the rows "Upload: …": a chunk out of order, the session's status, the
// cancel, a mount, a wrong digest and the empty blob; then a session continued by another
// provider.
func (r *run) measureUploads(ctx context.Context) error {
	const repo = "up"
	var s uploadSeen
	if err := r.chunks(ctx, repo, &s); err != nil {
		return err
	}
	r.set("chunkOrder", chunkOrderVerdict(s))
	r.set("sessionStatus", statusVerdict(s))
	if err := r.cancelled(r.in(ctx, "cancel"), repo, &s); err != nil {
		return err
	}
	r.set("cancel", cancelVerdict(s))
	if err := r.mounted(r.in(ctx, "mount"), repo, &s); err != nil {
		return err
	}
	r.set("mount", mountVerdict(s))
	if err := r.digests(r.in(ctx, "wrongDigest"), repo, &s); err != nil {
		return err
	}
	r.set("wrongDigest", wrongDigestVerdict(s))
	ctx = r.in(ctx, "replica")
	r.set("replica", r.replica(ctx))
	return nil
}

// patch sends one chunk with its Content-Range.
func (r *run) patch(ctx context.Context, loc *url.URL, data []byte, from int) (answer, error) {
	return r.c.do(ctx, request{method: http.MethodPatch, target: loc.String(), body: data, header: http.Header{
		"Content-Type": {octet}, "Content-Range": {fmt.Sprintf("%d-%d", from, from+len(data)-1)},
	}})
}

// next is the location an answer gives for the session's next request, loc without one.
func next(a answer, loc *url.URL) *url.URL {
	if l := a.location(); l != nil {
		return l
	}
	return loc
}

// chunks: abc at 0-2, the status, x at 7-7 out of order, then x at 3-3 and the closing PUT.
func (r *run) chunks(ctx context.Context, repo string, s *uploadSeen) error {
	loc, err := r.session(r.in(ctx, "chunkOrder"), repo)
	if err != nil {
		return err
	}
	if s.first, err = r.patch(ctx, loc, []byte("abc"), 0); err != nil {
		return err
	}
	if !s.first.ok(http.StatusAccepted, http.StatusNoContent) {
		return fmt.Errorf("a first chunk: %s", s.first.refusal())
	}
	loc = next(s.first, loc)
	if s.status, err = r.get(r.in(ctx, "sessionStatus"), loc.String(), nil); err != nil {
		return err
	}
	ctx = r.in(ctx, "chunkOrder")
	if s.outOfOrd, err = r.patch(ctx, loc, []byte("x"), 7); err != nil {
		return err
	}
	if s.outOfOrd.ok(http.StatusAccepted, http.StatusNoContent) {
		loc = next(s.outOfOrd, loc)
		if s.afterOdd, err = r.get(ctx, loc.String(), nil); err != nil {
			return err
		}
	} else {
		a, err := r.patch(ctx, loc, []byte("x"), 3)
		if err != nil {
			return err
		}
		loc = next(a, loc)
	}
	d := digest.FromBytes([]byte("abcx"))
	a, err := r.send(ctx, http.MethodPut, withDigest(loc, d))
	if err == nil && a.ok(http.StatusCreated) {
		r.blobs = append(r.blobs, pushed{repo, d})
	}
	return err
}

// chunkOrderVerdict is the cell "Upload: chunk out of order".
func chunkOrderVerdict(s uploadSeen) string {
	if !s.outOfOrd.ok(http.StatusAccepted, http.StatusNoContent) {
		return s.outOfOrd.refusal()
	}
	if s.afterOdd.header.Get("Range") == "0-3" {
		return "**appended**, no 416"
	}
	return fmt.Sprintf("**taken**: %d, `Range: %s`", s.outOfOrd.status, s.outOfOrd.header.Get("Range"))
}

// statusVerdict is the cell "Upload: session status (`GET`)".
func statusVerdict(s uploadSeen) string {
	want, got := s.first.header.Get("Range"), s.status.header.Get("Range")
	switch {
	case s.status.status/100 == 3:
		where := "elsewhere"
		if l := s.status.location(); l != nil {
			where = "`" + l.Host + l.Path + "`"
		}
		return fmt.Sprintf("**%d to %s**", s.status.status, where)
	case s.status.status != http.StatusNoContent && s.status.status != http.StatusOK:
		return s.status.refusal()
	case got == "":
		return fmt.Sprintf("%d without `Range`", s.status.status)
	case got == "0-3" && want == "0-2":
		return fmt.Sprintf("%d with `Range`: **one byte too many** (`%s` after `%s`)", s.status.status, got, want)
	case want != "" && got != want:
		return fmt.Sprintf("%d with `Range: %s`, the `PATCH` said `%s`", s.status.status, got, want)
	}
	return fmt.Sprintf("works, %d with `Range`", s.status.status)
}

// cancelled: a session holding abc, its DELETE, then its GET and a PATCH to it.
func (r *run) cancelled(ctx context.Context, repo string, s *uploadSeen) error {
	loc, err := r.session(ctx, repo)
	if err != nil {
		return err
	}
	a, err := r.patch(ctx, loc, []byte("abc"), 0)
	if err != nil {
		return err
	}
	loc = next(a, loc)
	if s.cancel, err = r.send(ctx, http.MethodDelete, loc.String()); err != nil {
		return err
	}
	if s.afterGet, err = r.get(ctx, loc.String(), nil); err != nil {
		return err
	}
	s.afterPut, err = r.patch(ctx, loc, []byte("d"), 3)
	return err
}

// cancelVerdict is the cell "Upload: cancel (`DELETE`)".
func cancelVerdict(s uploadSeen) string {
	if !s.cancel.ok(http.StatusNoContent, http.StatusAccepted, http.StatusOK) {
		return s.cancel.refusal()
	}
	v := fmt.Sprintf("works: %d, then its `GET` %s", s.cancel.status, statusText(s.afterGet))
	if s.afterPut.status >= 500 {
		v += fmt.Sprintf("; a `PATCH` after it **%d**", s.afterPut.status)
	}
	return v
}

// statusText is an answer's status, with its error code when it has one.
func statusText(a answer) string {
	if a.status < 400 {
		return fmt.Sprint(a.status)
	}
	return a.refusal()
}

// mounted pushes a blob to repo and asks to mount it into another repository.
func (r *run) mounted(ctx context.Context, repo string, s *uploadSeen) error {
	d, a, err := r.pushBlob(ctx, repo, []byte("lajstrom registry probe, a blob to mount "+r.id))
	if err != nil {
		return err
	}
	if !a.ok(http.StatusCreated) {
		return fmt.Errorf("push of a blob: %s", a.refusal())
	}
	q := url.Values{"mount": {d.String()}, "from": {r.name(repo)}}
	if s.mount, err = r.send(ctx, http.MethodPost, r.path("mount", "blobs/uploads/?"+q.Encode())); err != nil {
		return err
	}
	switch {
	case s.mount.ok(http.StatusCreated):
		r.blobs = append(r.blobs, pushed{"mount", d})
		s.mounted, err = r.c.do(ctx, request{method: http.MethodHead, target: r.path("mount", "blobs/"+d.String())})
	case s.mount.ok(http.StatusAccepted) && s.mount.location() != nil:
		_, err = r.send(ctx, http.MethodDelete, s.mount.location().String())
	}
	return err
}

// mountVerdict is the cell "Upload: mount (`POST ?mount=&from=`)".
func mountVerdict(s uploadSeen) string {
	switch {
	case s.mount.ok(http.StatusCreated) && s.mounted.status == http.StatusOK:
		return "works, 201"
	case s.mount.ok(http.StatusCreated):
		return "**201, but the blob is not there**: its `HEAD` " + statusText(s.mounted)
	case s.mount.ok(http.StatusAccepted):
		return "**202: a session instead**"
	}
	return s.mount.refusal()
}

// digests: a monolithic PUT whose digest is of other bytes, then the session and the wrong
// digest; then the empty blob.
func (r *run) digests(ctx context.Context, repo string, s *uploadSeen) error {
	loc, err := r.session(ctx, repo)
	if err != nil {
		return err
	}
	data := []byte("lajstrom registry probe " + r.id)
	wrong := digest.FromBytes([]byte("other bytes " + r.id))
	if s.wrong, err = r.c.do(ctx, request{method: http.MethodPut, target: withDigest(loc, wrong), body: data,
		header: http.Header{"Content-Type": {octet}}}); err != nil {
		return err
	}
	if s.wrong.ok(http.StatusCreated) {
		r.blobs = append(r.blobs, pushed{repo, wrong}, pushed{repo, digest.FromBytes(data)})
	}
	if s.wrongGet, err = r.get(ctx, loc.String(), nil); err != nil {
		return err
	}
	if s.wrongGet.ok(http.StatusNoContent, http.StatusOK) {
		_, _ = r.send(ctx, http.MethodDelete, loc.String())
	}
	if s.wrongHead, err = r.c.do(ctx, request{method: http.MethodHead, target: r.path(repo, "blobs/"+wrong.String())}); err != nil {
		return err
	}
	var d digest.Digest
	if d, s.empty, err = r.pushBlob(ctx, repo, []byte{}); err != nil {
		return err
	}
	s.emptyHead, err = r.c.do(ctx, request{method: http.MethodHead, target: r.path(repo, "blobs/"+d.String())})
	return err
}

// wrongDigestVerdict is the cell "Upload: wrong digest, empty blob".
func wrongDigestVerdict(s uploadSeen) string {
	var v string
	if s.wrong.ok(http.StatusCreated) {
		v = "**a wrong digest is 201**"
		if s.wrongHead.status == http.StatusOK {
			v += " **and stored under it**"
		}
	} else {
		v = s.wrong.refusal()
		switch {
		case s.wrongGet.ok(http.StatusNoContent, http.StatusOK):
			v += ", the session stays"
		case s.wrongGet.status == http.StatusNotFound:
			v += ", the session ends"
		default:
			v += ", the session then " + statusText(s.wrongGet)
		}
	}
	switch {
	case s.empty.ok(http.StatusCreated) && s.emptyHead.status == http.StatusOK:
		return v + "; the empty blob stored"
	case s.empty.ok(http.StatusCreated):
		return v + "; the empty blob taken, then " + statusText(s.emptyHead)
	}
	return v + "; the empty blob **refused**: " + s.empty.refusal()
}

// replica continues an upload session that one generic provider started on a second one, as
// two replicas of Lajstrom behind one address do (the facade keeps the session in its index):
// a chunk streamed from a reader that cannot be rewound, and the closing PUT.
func (r *run) replica(ctx context.Context) string {
	data := []byte("lajstrom registry probe, another replica " + r.id)
	d := digest.FromBytes(data)
	var repos [2]provider.Repository
	for i := range repos {
		p, err := generic.New(r.cfg, slog.New(&providerLog{next: r.opts.Log.Handler(), vendor: new(string)}))
		if err != nil {
			return "fails: " + err.Error()
		}
		if repos[i], err = p.Repository(ctx, r.rel+"replica"); err != nil {
			return "fails: " + err.Error()
		}
	}
	u, err := repos[0].StartBlobUpload(ctx)
	if err != nil {
		return "fails: " + err.Error()
	}
	if u, err = repos[1].AppendBlobChunk(ctx, u, onlyReader{bytes.NewReader(data)}, int64(len(data))); err != nil {
		_ = repos[1].CancelBlobUpload(ctx, u)
		return "fails: " + err.Error()
	}
	if _, err := repos[1].FinishBlobUpload(ctx, u, d, nil, 0); err != nil {
		return "fails: " + err.Error()
	}
	r.blobs = append(r.blobs, pushed{"replica", d})
	return "works (the provider: one starts the session, another streams a chunk and closes it)"
}

// onlyReader hides every method of a reader but Read, so that the request body cannot be
// rewound: the facade streams its client's chunk.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }
