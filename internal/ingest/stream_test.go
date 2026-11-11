package ingest_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // the md5 a plugin asks for
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// streamPlugin is rawPlugin as a format.Streamer: Inspect returns inspection, or inspectErr, and
// Build gets a streamed file without Open, which buildRaw describes by its sha256.
type streamPlugin struct {
	*rawPlugin
	inspection format.Inspection
	inspectErr error
	got        []format.UploadedFile // the files Build got
}

func (p *streamPlugin) Inspect(index.Repo, format.StreamUpload) (format.Inspection, error) {
	return p.inspection, p.inspectErr
}

func (p *streamPlugin) Build(ctx context.Context, repo index.Repo, up format.Upload) (*format.Built, error) {
	p.got = append(p.got, up.Files...)
	return p.rawPlugin.Build(ctx, repo, up)
}

// inspector records what it was written and whether it was closed; its Write fails with
// failWrite once it holds 8 bytes, its Close with failClose.
type inspector struct {
	bytes.Buffer
	closed               bool
	failWrite, failClose error
}

func (i *inspector) Write(p []byte) (int, error) {
	if i.failWrite != nil && i.Len()+len(p) > 8 {
		return 0, i.failWrite
	}
	return i.Buffer.Write(p)
}

func (i *inspector) Close() error { i.closed = true; return i.failClose }

// newStreamEnv is newEnv with the plugin a streamPlugin and the fake's chunks of 4 bytes, so
// that a file of a few bytes takes several PATCHes.
func newStreamEnv(t *testing.T) (*env, *streamPlugin) {
	t.Helper()
	e := newEnv(t)
	e.prov.ChunkSize = 4
	sp := &streamPlugin{rawPlugin: e.plugin}
	o := e.options(e.prov)
	o.Plugins = func(name string) (format.Plugin, bool) { return sp, name == "raw" }
	e.pipe = ingest.New(o)
	return e, sp
}

// streamUpload is the StreamUpload of content as the file docs/notes.txt of the package
// docs/notes.txt at version.
func streamUpload(version, content string) format.StreamUpload {
	return format.StreamUpload{Name: "docs/notes.txt", Body: strings.NewReader(content),
		File:   format.UploadedFile{Name: "docs/notes.txt", Size: int64(len(content)), ContentType: "text/plain"},
		Params: map[string]string{"name": "docs/notes.txt", "version": version}}
}

// stored reports whether the fake holds content as a blob of docs/notes.txt's repository.
func (e *env) stored(t *testing.T, content string) bool {
	t.Helper()
	rr, err := e.prov.Repository(t.Context(), "raw/scratch/docs/notes.txt")
	must(t, err)
	ok, err := rr.Exists(t.Context(), ocispec.Descriptor{Digest: digest.FromString(content), Size: int64(len(content))})
	must(t, err)
	return ok
}

// TestUploadStream: a file streams into its session in chunks, through sha256, the checksums
// the plugin asks for and its inspector, and Build gets it without Open; the session is
// committed and the manifest pushed. The same bytes again are the idempotent repeat, whose
// session is cancelled.
func TestUploadStream(t *testing.T) {
	ctx := t.Context()
	e, sp := newStreamEnv(t)
	insp := &inspector{}
	sp.inspection = format.Inspection{Checksums: []string{"md5"}, Inspector: insp}
	const content = "streamed in three chunks"

	pub, err := e.pipe.UploadStream(ctx, e.repo, streamUpload("1.0.0", content), alice)
	must(t, err)
	if pub.Existed || pub.Tag != "1.0.0" || pub.RepoPath != "raw/scratch/docs/notes.txt" {
		t.Errorf("Published = %+v", pub)
	}
	if len(sp.got) != 1 {
		t.Fatalf("Build got %d files, want 1", len(sp.got))
	}
	f := sp.got[0]
	md5sum := md5.Sum([]byte(content)) //nolint:gosec // the md5 the plugin asked for
	same(t, "the file Build got", map[string]any{"open": false, "size": int64(len(content)), "contentType": "text/plain",
		"checksums": map[string]string{"sha256": digest.FromString(content).Encoded(), "md5": fmt.Sprintf("%x", md5sum)}},
		map[string]any{"open": f.Open != nil, "size": f.Size, "contentType": f.ContentType, "checksums": f.Checksums})
	if insp.String() != content || !insp.closed || f.Inspector != insp {
		t.Errorf("the inspector saw %q, closed %v; Build got it: %v", insp.String(), insp.closed, f.Inspector == insp)
	}
	e.wantPublished(t, content, 1)
	if r := e.rows(t); len(r.Versions) != 1 || r.Files[0][0].Digest != digest.FromString(content) {
		t.Errorf("rows %+v", r)
	}

	again, err := e.pipe.UploadStream(ctx, e.repo, streamUpload("1.0.0", content), alice)
	must(t, err)
	if !again.Existed || again.Digest != pub.Digest {
		t.Errorf("repeat = %+v, want Existed with %s", again, pub.Digest)
	}
	e.wantPublished(t, content, 1)
}

// wantPublished fails the test unless the fake holds content, no session is open, and pushes
// manifests were pushed under the tag 1.0.0.
func (e *env) wantPublished(t *testing.T, content string, pushes int) {
	t.Helper()
	if !e.stored(t, content) || e.prov.OpenUploads() != 0 || len(e.pushRefs("1.0.0")) != pushes {
		t.Errorf("stored %v, %d sessions open, %d manifests pushed; want stored, none, %d",
			e.stored(t, content), e.prov.OpenUploads(), len(e.pushRefs("1.0.0")), pushes)
	}
}

// TestUploadStreamRefused: an upload refused at any step, while the body streams, by the
// inspector, by Build, by the checks after it, or by the registry at the commit, stores
// nothing: no blob, no session left open, no manifest, no claim.
func TestUploadStreamRefused(t *testing.T) {
	const body = "refused after a chunk or two"
	tooLarge := &http.MaxBytesError{Limit: 8}
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, e *env, sp *streamPlugin, up *format.StreamUpload)
		want  func(error) bool
	}{
		{"the body breaks off past maxFileSize", func(_ *testing.T, _ *env, _ *streamPlugin, up *format.StreamUpload) {
			up.Body = io.MultiReader(strings.NewReader(body[:9]), iotest.ErrReader(tooLarge))
		}, func(err error) bool { return errors.As(err, &tooLarge) && !errors.Is(err, ingest.ErrInvalid) }},
		{"the body is shorter than Content-Length", func(_ *testing.T, _ *env, _ *streamPlugin, up *format.StreamUpload) {
			up.File.Size++
		}, isInvalid},
		{"the inspector refuses a write", func(_ *testing.T, _ *env, sp *streamPlugin, _ *format.StreamUpload) {
			sp.inspection.Inspector = &inspector{failWrite: errors.New("not a tarball")}
		}, isInvalid},
		{"the inspector refuses the end", func(_ *testing.T, _ *env, sp *streamPlugin, _ *format.StreamUpload) {
			sp.inspection.Inspector = &inspector{failClose: errors.New("no package.json")}
		}, isInvalid},
		{"Build refuses", func(_ *testing.T, _ *env, _ *streamPlugin, up *format.StreamUpload) {
			up.Params["name"] = "docs//notes.txt"
		}, isInvalid},
		{"the policy's maxFileSize, checked after the stream", func(t *testing.T, e *env, _ *streamPlugin, _ *format.StreamUpload) {
			e.setPolicy(t, `{"maxFileSize":8}`)
		}, func(err error) bool { return isInvalid(err) && errors.As(err, &tooLarge) }},
		{"the tag holds other files", func(t *testing.T, e *env, _ *streamPlugin, _ *format.StreamUpload) {
			_, err := e.pipe.UploadStream(t.Context(), e.repo, streamUpload("1.0.0", "the first"), alice)
			must(t, err)
		}, func(err error) bool { return errors.Is(err, ingest.ErrConflict) }},
		{"the registry gets other bytes than were hashed", func(_ *testing.T, e *env, _ *streamPlugin, _ *format.StreamUpload) {
			e.pipe = ingest.New(e.optionsWith(corrupting{e.prov}))
		}, func(err error) bool { return errors.Is(err, content.ErrMismatchedDigest) && !isInvalid(err) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, sp := newStreamEnv(t)
			up := streamUpload("1.0.0", body)
			tt.setup(t, e, sp, &up)
			pushed, blobs := len(e.prov.PushReferences()), len(e.prov.Pushes())
			_, err := e.pipe.UploadStream(t.Context(), e.repo, up, alice)
			if err == nil || !tt.want(err) {
				t.Fatalf("UploadStream = %v", err)
			}
			if e.stored(t, body) || e.prov.OpenUploads() != 0 || len(e.prov.PushReferences()) != pushed || len(e.prov.Pushes()) != blobs {
				t.Errorf("stored %v, %d sessions open, %d manifests and %d blobs pushed; want nothing stored, none open, %d and %d pushed",
					e.stored(t, body), e.prov.OpenUploads(), len(e.prov.PushReferences()), len(e.prov.Pushes()), pushed, blobs)
			}
			e.noClaim(t, "docs/notes.txt", "1.0.0")
		})
	}
}

// TestUploadStreamValidateFetch: Validate's fetch of the streamed file, committed after it, is
// format.ErrStreamed, which a plugin tells from a failure of the registry; the other blobs,
// held in memory until the commit, read back as Build made them.
func TestUploadStreamValidateFetch(t *testing.T) {
	e, _ := newStreamEnv(t)
	const content = "read by Validate"
	var fileErr error
	var held []string
	e.plugin.ValidateFunc = func(ctx context.Context, repo index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
		for _, l := range append([]ocispec.Descriptor{m.Config}, m.Layers...) {
			rc, err := fetch(ctx, l.Digest)
			if l.Digest == digest.FromString(content) {
				fileErr = err
				continue
			}
			must(t, err)
			b, err := io.ReadAll(rc)
			_ = rc.Close()
			must(t, err)
			held = append(held, string(b))
		}
		return validateRaw(ctx, repo, m, fetch)
	}
	_, err := e.pipe.UploadStream(t.Context(), e.repo, streamUpload("1.0.0", content), alice)
	must(t, err)
	if !errors.Is(fileErr, format.ErrStreamed) || len(held) != 2 || held[0] != "{}" {
		t.Errorf("Validate's fetch of the file = %v, of the others %q; want format.ErrStreamed, the config and the metadata", fileErr, held)
	}
	e.wantPublished(t, content, 1)
}

// TestUploadStreamClosesInspector: the inspector of a body that fails, or that it refuses, is
// closed, so that it releases what it holds.
func TestUploadStreamClosesInspector(t *testing.T) {
	const content = "a body of a few chunks"
	for _, tt := range []struct {
		name string
		insp *inspector
		body io.Reader
	}{
		{"the body fails", &inspector{}, io.MultiReader(strings.NewReader(content[:9]), iotest.ErrReader(errors.New("reset")))},
		{"the inspector refuses a write", &inspector{failWrite: errors.New("not a tarball")}, strings.NewReader(content)},
	} {
		e, sp := newStreamEnv(t)
		sp.inspection = format.Inspection{Inspector: tt.insp}
		up := streamUpload("1.0.0", content)
		up.Body, up.File.Size = tt.body, -1
		if _, err := e.pipe.UploadStream(t.Context(), e.repo, up, alice); err == nil || !tt.insp.closed {
			t.Errorf("%s: UploadStream = %v, the inspector closed %v; want an error, closed", tt.name, err, tt.insp.closed)
		}
	}
}

// TestUploadStreamInspectorPanics: a format.ReadInspector whose parser panics on its goroutine
// makes UploadStream panic on the caller's, where the server's recovery answers 500, and the
// session is cancelled on the way: nothing stored, none open.
func TestUploadStreamInspectorPanics(t *testing.T) {
	const content = "a body of a few chunks"
	e, sp := newStreamEnv(t)
	sp.inspection = format.Inspection{Inspector: format.ReadInspector(func(r io.Reader) error {
		_, _ = io.ReadFull(r, make([]byte, 5))
		panic("the parser's bug")
	})}
	func() {
		defer func() {
			if v := recover(); v == nil || !strings.Contains(fmt.Sprint(v), "the parser's bug") {
				t.Errorf("UploadStream panicked with %v, want the parser's panic", v)
			}
		}()
		_, err := e.pipe.UploadStream(t.Context(), e.repo, streamUpload("1.0.0", content), alice)
		t.Errorf("UploadStream = %v, want a panic", err)
	}()
	if e.stored(t, content) || e.prov.OpenUploads() != 0 {
		t.Errorf("the file stored %v, %d sessions open; want nothing", e.stored(t, content), e.prov.OpenUploads())
	}
}

// TestUploadStreamNotStreamer: a plugin that is not a Streamer is ErrUnsupported, and a name it
// does not allow ErrInvalid, before anything reaches the registry.
func TestUploadStreamNotStreamer(t *testing.T) {
	e := newEnv(t)
	if _, err := e.pipe.UploadStream(t.Context(), e.repo, streamUpload("1.0.0", "x"), alice); !errors.Is(err, format.ErrUnsupported) {
		t.Errorf("UploadStream to a plugin that is not a Streamer = %v, want ErrUnsupported", err)
	}
	e, _ = newStreamEnv(t)
	up := streamUpload("1.0.0", "x")
	up.Name = "docs//notes.txt"
	if _, err := e.pipe.UploadStream(t.Context(), e.repo, up, alice); !isInvalid(err) {
		t.Errorf("UploadStream of a name the plugin refuses = %v, want ErrInvalid", err)
	}
	if e.prov.OpenUploads() != 0 {
		t.Errorf("%d sessions open", e.prov.OpenUploads())
	}
}

func isInvalid(err error) bool { return errors.Is(err, ingest.ErrInvalid) }

// optionsWith are the env's options with prov as the registry and its streamPlugin kept.
func (e *env) optionsWith(prov provider.Provider) ingest.Options {
	o := e.options(prov)
	sp := &streamPlugin{rawPlugin: e.plugin}
	o.Plugins = func(name string) (format.Plugin, bool) { return sp, name == "raw" }
	return o
}

// corrupting is the fake as a registry whose chunks arrive with their first byte changed.
type corrupting struct{ *providertest.Fake }

func (c corrupting) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := c.Fake.Repository(ctx, name)
	return corruptingRepo{rr}, err
}

type corruptingRepo struct{ provider.Repository }

func (r corruptingRepo) AppendBlobChunk(ctx context.Context, u provider.Upload, rd io.Reader, size int64) (provider.Upload, error) {
	b, err := io.ReadAll(rd)
	if err != nil {
		return u, err
	}
	b[0] ^= 0xff
	return r.Repository.AppendBlobChunk(ctx, u, bytes.NewReader(b), size)
}

func (r corruptingRepo) OpenBlobWriter(ctx context.Context) (provider.BlobWriter, error) {
	return provider.NewChunkedWriter(ctx, r, 4), nil
}
