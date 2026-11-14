package providertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
)

const name = "raw/scratch/tool"

func descriptor(mediaType string, b []byte) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}
}

// newRepo returns a new Fake and its repository name.
func newRepo(t *testing.T) (*Fake, provider.Repository) {
	t.Helper()
	f := &Fake{}
	rr, err := f.Repository(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return f, rr
}

// TestFakeBlobs checks that pushes are verified, that a small blob and an empty one read back,
// and that a blob above MaxKept exists but is not kept.
func TestFakeBlobs(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	small := []byte("hello")
	smallDesc := descriptor("application/octet-stream", small)
	if err := rr.Push(ctx, smallDesc, strings.NewReader("hellO")); err == nil {
		t.Error("Push of other bytes than the descriptor's succeeded")
	}
	if err := rr.Push(ctx, smallDesc, bytes.NewReader(small)); err != nil {
		t.Fatal(err)
	}
	if rc, err := rr.Fetch(ctx, smallDesc); err != nil {
		t.Error(err)
	} else if got, _ := io.ReadAll(rc); !bytes.Equal(got, small) {
		t.Errorf("Fetch = %q", got)
	}
	emptyDesc := descriptor("application/octet-stream", nil)
	if err := rr.Push(ctx, emptyDesc, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	if rc, err := rr.Fetch(ctx, emptyDesc); err != nil {
		t.Errorf("Fetch of an empty blob: %v", err)
	} else if got, _ := io.ReadAll(rc); len(got) != 0 {
		t.Errorf("Fetch of an empty blob = %q", got)
	}

	big := ocispec.Descriptor{MediaType: "application/octet-stream", Size: MaxKept + 1}
	d := digest.Canonical.Digester()
	_, _ = io.Copy(d.Hash(), io.LimitReader(zeros{}, big.Size))
	big.Digest = d.Digest()
	if err := rr.Push(ctx, big, io.LimitReader(zeros{}, big.Size)); err != nil {
		t.Fatal(err)
	}
	if ok, err := rr.Exists(ctx, big); !ok || err != nil {
		t.Errorf("Exists(big) = %v, %v", ok, err)
	}
	if _, err := rr.Fetch(ctx, big); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Fetch of a discarded blob: %v, want ErrNotFound", err)
	}
	if got := len(f.Pushes()); got != 3 {
		t.Errorf("%d pushes recorded, want 3", got)
	}
	if names, _ := f.ListRepositories(ctx, "raw/"); !slices.Equal(names, []string{name}) {
		t.Errorf("ListRepositories = %v", names)
	}
}

// TestFakeManifests checks tags, referrers, the record of PushReference and Delete.
func TestFakeManifests(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	m, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest})
	mDesc := descriptor(ocispec.MediaTypeImageManifest, m)
	if err := rr.PushReference(ctx, mDesc, bytes.NewReader(m), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	sig, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, ArtifactType: "application/vnd.test.sig", Subject: &mDesc})
	sigDesc := descriptor(ocispec.MediaTypeImageManifest, sig)
	if err := rr.Push(ctx, sigDesc, bytes.NewReader(sig)); err != nil {
		t.Fatal(err)
	}
	if got, err := rr.Resolve(ctx, "1.0.0"); err != nil || got.Digest != mDesc.Digest {
		t.Errorf("Resolve = %v, %v", got, err)
	}
	var refs []ocispec.Descriptor
	_ = rr.Referrers(ctx, mDesc, "", func(page []ocispec.Descriptor) error { refs = append(refs, page...); return nil })
	if len(refs) != 1 || refs[0].Digest != sigDesc.Digest || refs[0].ArtifactType != "application/vnd.test.sig" {
		t.Errorf("Referrers = %v", refs)
	}
	if got := f.PushReferences(); !slices.Equal(got, []PushRecord{{Name: name, Tag: "1.0.0", Digest: mDesc.Digest, Size: mDesc.Size}}) {
		t.Errorf("PushReferences = %v", got)
	}
	if err := rr.Delete(ctx, mDesc); err != nil {
		t.Fatal(err)
	}
	if _, err := rr.Resolve(ctx, "1.0.0"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Resolve after Delete: %v, want ErrNotFound", err)
	}
}

// TestFakeStatManifest checks StatManifest: a manifest it has, MANIFEST_UNKNOWN in a repository
// it has, NAME_UNKNOWN in one it does not, and FailStatManifest.
func TestFakeStatManifest(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	m, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest})
	desc := descriptor(ocispec.MediaTypeImageManifest, m)
	if err := rr.PushReference(ctx, desc, bytes.NewReader(m), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if got, err := f.StatManifest(ctx, name, desc.Digest); err != nil || got.Digest != desc.Digest || got.Size != desc.Size || got.MediaType != desc.MediaType {
		t.Errorf("StatManifest = %v, %v", got, err)
	}
	if err := rr.Delete(ctx, desc); err != nil {
		t.Fatal(err)
	}
	if _, err := f.StatManifest(ctx, name, desc.Digest); !errors.Is(err, provider.ErrManifestUnknown) {
		t.Errorf("StatManifest after Delete: %v, want ErrManifestUnknown", err)
	}
	if _, err := f.StatManifest(ctx, "raw/scratch/none", desc.Digest); !errors.Is(err, provider.ErrNotFound) ||
		errors.Is(err, provider.ErrManifestUnknown) {
		t.Errorf("StatManifest in a repository the registry does not have: %v, want ErrNotFound alone", err)
	}
	boom := errors.New("boom")
	f.FailStatManifest = func(string, digest.Digest) error { return boom }
	if _, err := f.StatManifest(ctx, name, desc.Digest); !errors.Is(err, boom) {
		t.Errorf("StatManifest with FailStatManifest: %v", err)
	}
}

// TestFakeFaults checks the injected errors.
func TestFakeFaults(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	boom := errors.New("boom")
	f.FailPush, f.FailPushReference, f.FailExists = boom, boom, boom
	b := []byte("x")
	desc := descriptor(ocispec.MediaTypeImageManifest, b)
	for what, err := range map[string]error{
		"Push":          rr.Push(ctx, desc, bytes.NewReader(b)),
		"PushReference": rr.PushReference(ctx, desc, bytes.NewReader(b), "1.0.0"),
		"Exists":        second(rr.Exists(ctx, desc)),
	} {
		if !errors.Is(err, boom) {
			t.Errorf("%s with its Fail field set: %v", what, err)
		}
	}
	if len(f.Pushes()) != 0 || len(f.PushReferences()) != 0 {
		t.Errorf("failed pushes were recorded: %v %v", f.Pushes(), f.PushReferences())
	}
}

// TestFakeFailRequests checks FailRequests: every call that sends the registry a request fails
// with it alone, before the other Fail fields, and changes nothing; Name, Repository and
// EnsureRepository, which send none, answer; a BlobWriter fails at Commit, its first request.
func TestFakeFailRequests(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	b := []byte("blob")
	desc := descriptor("application/octet-stream", b)
	m, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest})
	mDesc := descriptor(ocispec.MediaTypeImageManifest, m)
	u := seed(t, rr, b, m) // what each call would find, so that one that ignored FailRequests would succeed
	down, other := errors.New("down"), errors.New("other")
	f.FailRequests = down
	f.FailPush, f.FailUpload, f.FailPushReference, f.FailExists, f.FailFetch = other, other, other, other, other
	f.FailStatManifest = func(string, digest.Digest) error { return other }
	for what, call := range map[string]func() error{
		"Capabilities":     func() error { return second(f.Capabilities(ctx)) },
		"ListRepositories": func() error { return second(f.ListRepositories(ctx, "")) },
		"DeleteRepository": func() error { return f.DeleteRepository(ctx, name) },
		"OpenBlob":         func() error { return second(f.OpenBlob(ctx, name, desc.Digest, "", true)) },
		"StatBlob":         func() error { return second(f.StatBlob(ctx, name, desc.Digest)) },
		"StatManifest":     func() error { return second(f.StatManifest(ctx, name, mDesc.Digest)) },
		"Fetch":            func() error { return second(rr.Fetch(ctx, desc)) },
		"Exists":           func() error { return second(rr.Exists(ctx, desc)) },
		"Push":             func() error { return rr.Push(ctx, desc, bytes.NewReader(b)) },
		"PushReference":    func() error { return rr.PushReference(ctx, mDesc, bytes.NewReader(m), "2.0.0") },
		"Delete":           func() error { return rr.Delete(ctx, mDesc) },
		"Tag":              func() error { return rr.Tag(ctx, mDesc, "2.0.0") },
		"Resolve":          func() error { return second(rr.Resolve(ctx, "1.0.0")) },
		"Tags":             func() error { return rr.Tags(ctx, "", func([]string) error { return nil }) },
		"Referrers":        func() error { return rr.Referrers(ctx, mDesc, "", func([]ocispec.Descriptor) error { return nil }) },
		"StartBlobUpload":  func() error { return second(rr.StartBlobUpload(ctx)) },
		"MountBlob":        func() error { _, _, err := rr.MountBlob(ctx, desc.Digest, name); return err },
		"AppendBlobChunk":  func() error { return second(rr.AppendBlobChunk(ctx, u, bytes.NewReader(b), int64(len(b)))) },
		"FinishBlobUpload": func() error {
			return second(rr.FinishBlobUpload(ctx, u, desc.Digest, bytes.NewReader(b), int64(len(b))))
		},
		"BlobUploadStatus":  func() error { return second(rr.BlobUploadStatus(ctx, u)) },
		"CancelBlobUpload":  func() error { return rr.CancelBlobUpload(ctx, u) },
		"BlobWriter.Commit": func() error { return writeBlob(ctx, rr, b) },
	} {
		if err := call(); !errors.Is(err, down) || errors.Is(err, other) {
			t.Errorf("%s with FailRequests set: %v, want its error alone", what, err)
		}
	}
	if _, err := f.Repository(ctx, name); err != nil || f.Name() != "fake" || f.EnsureRepository(ctx, name) != nil {
		t.Errorf("Name, Repository or EnsureRepository failed with FailRequests set: %v", err)
	}
	f.FailRequests, f.FailExists, f.FailFetch, f.FailStatManifest = nil, nil, nil, nil
	if got, err := rr.Resolve(ctx, "1.0.0"); err != nil || got.Digest != mDesc.Digest {
		t.Errorf("Resolve after FailRequests = %v, %v; want the manifest the failed calls left", got, err)
	}
	if ok, err := rr.Exists(ctx, desc); !ok || err != nil || len(f.Pushes()) != 1 || len(f.PushReferences()) != 1 || f.OpenUploads() != 1 {
		t.Errorf("the failed calls changed the Fake: Exists %v, %v; %d pushes, %d push references, %d open uploads",
			ok, err, len(f.Pushes()), len(f.PushReferences()), f.OpenUploads())
	}
}

// seed pushes the blob b and the manifest m, tagged 1.0.0, to rr, and opens an upload session.
func seed(t *testing.T, rr provider.Repository, b, m []byte) provider.Upload {
	t.Helper()
	ctx := t.Context()
	if err := rr.Push(ctx, descriptor("application/octet-stream", b), bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if err := rr.PushReference(ctx, descriptor(ocispec.MediaTypeImageManifest, m), bytes.NewReader(m), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	u, err := rr.StartBlobUpload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// writeBlob writes b through a BlobWriter of rr and commits it.
func writeBlob(ctx context.Context, rr provider.Repository, b []byte) error {
	w, err := rr.OpenBlobWriter(ctx)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	return second(w.Commit(ctx, digest.FromBytes(b)))
}

// TestFakeOpenBlob checks OpenBlob: the whole blob, a single range in its three forms, a range
// past the end, the Ranges it ignores, and the record of every call.
func TestFakeOpenBlob(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	b := []byte("0123456789")
	desc := descriptor("application/octet-stream", b)
	if err := rr.Push(ctx, desc, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		rng          string
		status       int
		body         string
		contentRange string
	}{
		{"", http.StatusOK, "0123456789", ""},
		{"bytes=2-4", http.StatusPartialContent, "234", "bytes 2-4/10"},
		{"bytes=7-", http.StatusPartialContent, "789", "bytes 7-9/10"},
		{"bytes=8-20", http.StatusPartialContent, "89", "bytes 8-9/10"},
		{"bytes=-3", http.StatusPartialContent, "789", "bytes 7-9/10"},
		{"bytes=-30", http.StatusPartialContent, "0123456789", "bytes 0-9/10"},
		{"bytes=10-", http.StatusRequestedRangeNotSatisfiable, "", "bytes */10"},
		{"bytes=0-1,4-5", http.StatusOK, "0123456789", ""},
		{"bytes=4-2", http.StatusOK, "0123456789", ""},
		{"bytes=x-", http.StatusOK, "0123456789", ""},
		{"bytes=-0", http.StatusOK, "0123456789", ""},
		{"bytes=-x", http.StatusOK, "0123456789", ""},
		{"bytes=2-x", http.StatusOK, "0123456789", ""},
		{"bytes=5", http.StatusOK, "0123456789", ""},
		{"items=1-2", http.StatusOK, "0123456789", ""},
	}
	for _, tt := range tests {
		got, err := f.OpenBlob(ctx, name, desc.Digest, tt.rng, true)
		if err != nil {
			t.Fatalf("OpenBlob(%q): %v", tt.rng, err)
		}
		body := readBody(got)
		if got.Status != tt.status || body != tt.body || got.ContentRange != tt.contentRange || got.Digest != desc.Digest ||
			!got.AcceptRanges || got.Size != int64(len(tt.body)) || got.RedirectURL != "" {
			t.Errorf("OpenBlob(%q) = %+v with %q, want %d %q %q", tt.rng, got, body, tt.status, tt.body, tt.contentRange)
		}
		if tt.status == http.StatusRequestedRangeNotSatisfiable && got.Body != nil {
			t.Errorf("OpenBlob(%q) has a body", tt.rng)
		}
	}
	if len(f.OpenBlobs()) != len(tests) || f.OpenBlobs()[1] != (BlobRead{Name: name, Digest: desc.Digest, Range: "bytes=2-4"}) {
		t.Errorf("OpenBlobs = %+v", f.OpenBlobs())
	}
}

// TestFakeBlobMissing checks OpenBlob and StatBlob of blobs the repository does not have, of
// one it has, and of one it discarded on push, and the record of StatBlob.
func TestFakeBlobMissing(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	b := []byte("0123456789")
	desc := descriptor("application/octet-stream", b)
	if err := rr.Push(ctx, desc, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{name, "raw/scratch/other"} {
		if _, err := f.OpenBlob(ctx, n, digest.FromString("missing"), "", true); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("OpenBlob of a missing blob in %s: %v, want ErrNotFound", n, err)
		}
		if _, err := f.StatBlob(ctx, n, digest.FromString("missing")); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("StatBlob of a missing blob in %s: %v, want ErrNotFound", n, err)
		}
	}
	if st, err := f.StatBlob(ctx, name, desc.Digest); err != nil || st != (provider.Blob{Status: http.StatusOK, Size: 10, Digest: desc.Digest, AcceptRanges: true}) {
		t.Errorf("StatBlob = %+v, %v", st, err)
	}
	if got := f.StatBlobs(); len(got) != 3 || got[2] != (BlobRead{Name: name, Digest: desc.Digest}) {
		t.Errorf("StatBlobs = %+v", got)
	}

	big := ocispec.Descriptor{MediaType: "application/octet-stream", Size: MaxKept + 1}
	d := digest.Canonical.Digester()
	_, _ = io.Copy(d.Hash(), io.LimitReader(zeros{}, big.Size))
	big.Digest = d.Digest()
	if err := rr.Push(ctx, big, io.LimitReader(zeros{}, big.Size)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.OpenBlob(ctx, name, big.Digest, "", true); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("OpenBlob of a discarded blob: %v, want ErrNotFound", err)
	}
	if st, err := f.StatBlob(ctx, name, big.Digest); err != nil || st.Size != big.Size {
		t.Errorf("StatBlob of a discarded blob = %+v, %v; want its size", st, err)
	}
}

// TestFakeRedirect checks RedirectURL: no redirect by default; the function's URL when it
// says so, unless OpenBlob's redirect is false or NoBlobRedirect is set; ErrNotFound for a
// blob the repository does not have, before RedirectURL is asked.
func TestFakeRedirect(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	b := []byte("blob")
	desc := descriptor("application/octet-stream", b)
	if err := rr.Push(ctx, desc, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	stream := func(what string, got provider.Blob, err error) {
		t.Helper()
		if err != nil || got.RedirectURL != "" || got.Body == nil || got.Status != http.StatusOK {
			t.Errorf("%s = %+v, %v; want the stream", what, got, err)
		}
	}
	got, err := f.OpenBlob(ctx, name, desc.Digest, "", true)
	stream("OpenBlob without RedirectURL", got, err)
	if c, _ := f.Capabilities(ctx); c.BlobRedirect {
		t.Errorf("Capabilities without RedirectURL = %+v", c)
	}
	asked := 0
	f.RedirectURL = func(n string, d digest.Digest) (string, bool) {
		asked++
		return "https://cdn.test/" + n + "/" + d.Encoded(), d == desc.Digest
	}
	got, err = f.OpenBlob(ctx, name, desc.Digest, "bytes=1-2", true)
	want := provider.Blob{RedirectURL: "https://cdn.test/" + name + "/" + desc.Digest.Encoded(), Status: http.StatusTemporaryRedirect, Size: -1, Digest: desc.Digest}
	if err != nil || got != want {
		t.Errorf("OpenBlob = %+v, %v; want %+v", got, err, want)
	}
	got, err = f.OpenBlob(ctx, name, desc.Digest, "", false)
	stream("OpenBlob with redirect false", got, err)
	if _, err := f.OpenBlob(ctx, name, digest.FromString("missing"), "", true); !errors.Is(err, provider.ErrNotFound) || asked != 1 {
		t.Errorf("OpenBlob of a missing blob: %v, RedirectURL asked %d times, want ErrNotFound and once", err, asked)
	}
	if c, _ := f.Capabilities(ctx); !c.DeleteManifest || !c.BlobRedirect {
		t.Errorf("Capabilities = %+v, want DeleteManifest and BlobRedirect", c)
	}
	f.NoBlobRedirect = true
	got, err = f.OpenBlob(ctx, name, desc.Digest, "", true)
	stream("OpenBlob with NoBlobRedirect", got, err)
	if c, _ := f.Capabilities(ctx); c.BlobRedirect || asked != 1 {
		t.Errorf("Capabilities with NoBlobRedirect = %+v; RedirectURL asked %d times", c, asked)
	}
}

// TestFakeNoDeleteManifest checks a Fake without manifest deletes: it says so in Capabilities and
// refuses them, but still deletes blobs.
func TestFakeNoDeleteManifest(t *testing.T) {
	ctx := t.Context()
	f, rr := newRepo(t)
	b := []byte("blob")
	desc := descriptor("application/octet-stream", b)
	if err := rr.Push(ctx, desc, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	f.NoDeleteManifest = true
	if c, _ := f.Capabilities(ctx); c.DeleteManifest {
		t.Errorf("Capabilities with NoDeleteManifest = %+v", c)
	}
	m, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest})
	mDesc := descriptor(ocispec.MediaTypeImageManifest, m)
	if err := rr.PushReference(ctx, mDesc, bytes.NewReader(m), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := rr.Delete(ctx, mDesc); !errors.Is(err, provider.ErrUnsupported) {
		t.Errorf("Delete of a manifest with NoDeleteManifest: %v, want ErrUnsupported", err)
	}
	if _, err := rr.Resolve(ctx, "1.0.0"); err != nil {
		t.Errorf("the manifest is gone after a refused Delete: %v", err)
	}
	if err := rr.Delete(ctx, desc); err != nil {
		t.Errorf("Delete of a blob with NoDeleteManifest: %v", err)
	}
}

func second[T any](_ T, err error) error { return err }

// readBody reads the body of b; "" without one.
func readBody(b provider.Blob) string {
	if b.Body == nil {
		return ""
	}
	all, _ := io.ReadAll(b.Body)
	return string(all)
}

// zeros reads zero bytes forever.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
