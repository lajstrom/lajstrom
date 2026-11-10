package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	_ "github.com/lajstrom/lajstrom/internal/format/maven" // the plugin of a maven repo, for TestManifestHookSpellingMaven
	_ "github.com/lajstrom/lajstrom/internal/format/raw"   // the plugin of raw/assets, for the pipeline and rawRoutes
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// rawFile is a file of raw/assets as oras pushes it (docs/quickstart/oras.md): the file blob
// uploaded to the backend, and the manifest with every annotation, the file layer titled with
// the path and the empty config.
type rawFile struct {
	path    string
	content []byte
	digest  digest.Digest
	m       ocispec.Manifest
}

// newRawFile uploads content as the file at path, which must be its own package path (lower
// case, no escapes), and returns the file with its manifest.
func (e *env) newRawFile(path, content string) *rawFile {
	e.t.Helper()
	e.pushBlob("raw/assets/"+path, []byte(content))
	return rawFileOf(path, content)
}

// rawFileOf returns the file at path with content and its manifest, uploading nothing.
func rawFileOf(path, content string) *rawFile {
	d := digest.FromString(content)
	version := d.Encoded()[:16]
	return &rawFile{path: path, content: []byte(content), digest: d, m: ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: profile.ArtifactType("raw"),
		Config:       profile.EmptyConfig,
		Layers: []ocispec.Descriptor{{MediaType: profile.MediaTypeRawFile, Digest: d, Size: int64(len(content)),
			Annotations: map[string]string{ocispec.AnnotationTitle: path}}},
		Annotations: map[string]string{
			profile.AnnotationSchema: "1", profile.AnnotationFormat: "raw", profile.AnnotationRepo: "assets",
			profile.AnnotationName: path, profile.AnnotationVersion: version, profile.AnnotationRawPath: path,
			ocispec.AnnotationCreated: "2026-11-07T10:00:00Z",
		},
	}}
}

// tag is the tag of the file: its version, the first 16 hex digits of its sha256.
func (f *rawFile) tag() string { return f.digest.Encoded()[:16] }

// body returns the manifest's bytes and digest.
func (f *rawFile) body(t *testing.T) ([]byte, digest.Digest) {
	t.Helper()
	b, err := json.Marshal(f.m)
	must(t, err)
	return b, digest.FromBytes(b)
}

// put pushes body as the manifest of the file at ref, by alice, with the OCI manifest type
// unless hdr sets another.
func (e *env) put(f *rawFile, ref string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	hdr = append([]string{"Content-Type", ocispecManifest, principalHeader, "alice"}, hdr...)
	return e.do(e.h, http.MethodPut, "/v2/raw/assets/"+f.path+"/manifests/"+ref, bytes.NewReader(body), hdr...)
}

// rawRoutes are the routes of the raw plugin over st and providers, as the server mounts them.
func rawRoutes(t *testing.T, st index.Store, providers provider.Set) http.Handler {
	t.Helper()
	p, ok := format.Lookup("raw")
	if !ok {
		t.Fatal("the raw plugin is not registered")
	}
	deps := format.Deps{Index: st, Providers: providers, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	p.Routes(format.NewRouter(mux, "raw", deps), deps)
	return mux
}

// wantEntry fails the test when the answer is not status with one error of code, and returns
// the error.
func wantEntry(t *testing.T, what string, rr *httptest.ResponseRecorder, status int, code string) errorEntry {
	t.Helper()
	wantError(t, what, rr, status, code)
	var body errorBody
	if json.Unmarshal(rr.Body.Bytes(), &body) != nil || len(body.Errors) == 0 {
		return errorEntry{}
	}
	return body.Errors[0]
}

// TestManifestHook is the raw file of the quick start pushed as oras pushes it: indexed,
// served by the raw route, the repeat answered alike, another manifest under the tag refused.
func TestManifestHook(t *testing.T) {
	e := newEnv(t, envOptions{})
	f := e.newRawFile("docs/hello.txt", "hello lajstrom\n")
	body, d := f.body(t)
	rr := e.put(f, f.tag(), body)
	wantStatus(t, "push", rr, http.StatusCreated)
	wantHeader(t, "push", rr, "Location", "/v2/raw/assets/docs/hello.txt/manifests/"+d.String())
	wantHeader(t, "push", rr, "Docker-Content-Digest", d.String())
	wantHeader(t, "push", rr, "OCI-Subject", "")
	if refs := e.prov.PushReferences(); len(refs) != 1 || refs[0].Name != "raw/assets/docs/hello.txt" || refs[0].Tag != f.tag() || refs[0].Digest != d {
		t.Errorf("PushReferences = %+v", refs)
	}

	e.wantIndexed(f, d)
	get := e.do(rawRoutes(t, e.st, provider.Map{"local": e.reg}), http.MethodGet, "/raw/assets/docs/hello.txt", nil)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), f.content) {
		t.Errorf("GET /raw/assets/docs/hello.txt = %d %q", get.Code, get.Body)
	}

	rr = e.put(f, f.tag(), body)
	wantStatus(t, "the same manifest again", rr, http.StatusCreated)
	wantHeader(t, "the same manifest again", rr, "Docker-Content-Digest", d.String())
	if refs := e.prov.PushReferences(); len(refs) != 1 {
		t.Errorf("the repeat pushed again: %+v", refs)
	}
	e.wantIndexed(f, d) // one version, one audit event: the repeat wrote nothing

	// The same file built a second later is another manifest: through the facade the tag
	// keeps the first (manifest-profile.md §4.2).
	f.m.Annotations[ocispec.AnnotationCreated] = "2026-11-07T10:00:01Z"
	other, _ := f.body(t)
	entry := wantEntry(t, "another manifest under the tag", e.put(f, f.tag(), other), http.StatusConflict, codeTagInvalid)
	if !strings.Contains(entry.Message, d.String()) {
		t.Errorf("message %q does not name %s", entry.Message, d)
	}
	if detail, ok := entry.Detail.(map[string]any); !ok || detail["existing"] != d.String() {
		t.Errorf("detail %#v, want existing %s", entry.Detail, d)
	}
}

// wantIndexed checks the rows ingest wrote for f pushed by alice as the manifest d: the package,
// one version, its file and one audit event.
func (e *env) wantIndexed(f *rawFile, d digest.Digest) {
	e.t.Helper()
	ctx := e.t.Context()
	pkg, err := e.st.GetPackage(ctx, e.raw.ID, f.path)
	must(e.t, err)
	versions, err := e.st.ListVersions(ctx, pkg.ID)
	must(e.t, err)
	if len(versions) != 1 {
		e.t.Fatalf("versions = %+v", versions)
	}
	v := versions[0]
	if got, want := [4]any{pkg.NameOCI, v.Tag, v.Version, v.Digest}, [4]any{f.path, f.tag(), f.tag(), d}; got != want {
		e.t.Errorf("package path, tag, version, digest = %v, want %v", got, want)
	}
	files, err := e.st.ListFiles(ctx, v.ID)
	must(e.t, err)
	evs, err := e.st.ListAudit(ctx, index.AuditFilter{Action: "push"})
	must(e.t, err)
	if len(files) != 1 || len(evs) != 1 {
		e.t.Fatalf("files = %+v, audit = %+v", files, evs)
	}
	if got, want := [3]any{files[0].Name, files[0].Digest, files[0].MediaType}, [3]any{f.path, f.digest, profile.MediaTypeRawFile}; got != want {
		e.t.Errorf("file name, digest, media type = %v, want %v", got, want)
	}
	if got, want := [3]any{evs[0].Principal, evs[0].Repo, evs[0].Digest}, [3]any{userP("alice").String(), "raw/assets", d}; got != want {
		e.t.Errorf("audit principal, repo, digest = %v, want %v", got, want)
	}
}

// TestManifestHookInvalid covers what the hook and ingest refuse, each answer naming the key,
// the rule or the digest.
func TestManifestHookInvalid(t *testing.T) {
	e := newEnv(t, envOptions{})
	missing := digest.FromString("never uploaded")
	imageIndex := ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex}
	tests := []struct {
		name        string
		change      func(f *rawFile)
		ref         string // "" is the file's tag, "digest" the manifest's digest
		contentType string // "" is the OCI image manifest
		body        any    // nil is the file's manifest
		status      int
		code        string
		message     string // in the message
		detail      string // in the detail
	}{
		{name: "lajstrom.schema missing", change: func(f *rawFile) { delete(f.m.Annotations, profile.AnnotationSchema) },
			status: http.StatusBadRequest, code: codeManifestInvalid, message: "not a valid raw artifact", detail: "lajstrom.schema is missing"},
		{name: "lajstrom.version missing", change: func(f *rawFile) { delete(f.m.Annotations, profile.AnnotationVersion) },
			status: http.StatusBadRequest, code: codeManifestInvalid, detail: "lajstrom.version is missing"},
		{name: "lajstrom.repo of another repo", change: func(f *rawFile) { f.m.Annotations[profile.AnnotationRepo] = "other" },
			status: http.StatusBadRequest, code: codeManifestInvalid, detail: `lajstrom.repo does not match the repo: "other", want "assets"`},
		{name: "a tag that is not the version's", ref: "v1",
			status: http.StatusBadRequest, code: codeManifestInvalid, detail: "the tag of lajstrom.version and lajstrom.variant does not match the tag pushed to"},
		{name: "oras's default artifactType", change: func(f *rawFile) { f.m.ArtifactType = "application/vnd.unknown.artifact.v1" },
			status: http.StatusBadRequest, code: codeManifestInvalid, message: "pass --artifact-type application/vnd.lajstrom.raw.v1", detail: "artifactType"},
		{name: "the title oras gives a file name", change: func(f *rawFile) { f.m.Layers[0].Annotations[ocispec.AnnotationTitle] = "hello.txt" },
			status: http.StatusBadRequest, code: codeManifestInvalid, detail: `ingest: raw plugin: the file layer is titled "hello.txt"`},
		{name: "an image index", contentType: ocispec.MediaTypeImageIndex, body: imageIndex,
			status: http.StatusBadRequest, code: codeManifestInvalid, message: "an image index is not a Lajstrom artifact; an oci repo takes it"},
		{name: "a Docker manifest", contentType: "application/vnd.docker.distribution.manifest.v2+json",
			change: func(f *rawFile) { f.m.MediaType = "application/vnd.docker.distribution.manifest.v2+json" },
			status: http.StatusBadRequest, code: codeManifestInvalid, message: "is not a Lajstrom artifact: the profile wants " + ocispec.MediaTypeImageManifest},
		{name: "not a manifest", body: map[string]any{"mediaType": ocispec.MediaTypeImageManifest, "layers": "none"},
			status: http.StatusBadRequest, code: codeManifestInvalid, message: "not an OCI image manifest"},
		{name: "a layer not uploaded", change: func(f *rawFile) { f.m.Layers[0].Digest = missing },
			status: http.StatusBadRequest, code: codeManifestBlobUnknown, message: missing.String()},
		{name: "the file twice", change: func(f *rawFile) { f.m.Layers = append(f.m.Layers, f.m.Layers[0]) },
			status: http.StatusBadRequest, code: codeManifestInvalid, detail: "a raw manifest has one file layer, got 2"},
		{name: "a malformed layer digest", change: func(f *rawFile) { f.m.Layers[0].Digest = "sha256:short" },
			status: http.StatusBadRequest, code: codeManifestInvalid, detail: "layer descriptor"},
		{name: "by digest without lajstrom.version", ref: "digest", change: func(f *rawFile) { delete(f.m.Annotations, profile.AnnotationVersion) },
			status: http.StatusBadRequest, code: codeManifestInvalid, message: "a push by digest needs the lajstrom.version annotation; push with a tag"},
		{name: "by digest without lajstrom.schema", ref: "digest", change: func(f *rawFile) { delete(f.m.Annotations, profile.AnnotationSchema) },
			status: http.StatusBadRequest, code: codeManifestInvalid, detail: "lajstrom.schema is missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := e.newRawFile("docs/invalid.txt", "an invalid push")
			if tt.change != nil {
				tt.change(f)
			}
			body, d := f.body(t)
			if tt.body != nil {
				var err error
				body, err = json.Marshal(tt.body)
				must(t, err)
				d = digest.FromBytes(body)
			}
			ref := map[string]string{"": f.tag(), "digest": d.String()}[tt.ref]
			if ref == "" {
				ref = tt.ref
			}
			contentType := tt.contentType
			if contentType == "" {
				contentType = ocispecManifest
			}
			entry := wantEntry(t, tt.name, e.put(f, ref, body, "Content-Type", contentType), tt.status, tt.code)
			if !strings.Contains(entry.Message, tt.message) {
				t.Errorf("message %q does not contain %q", entry.Message, tt.message)
			}
			if detail := fmt.Sprint(entry.Detail); tt.detail != "" && !strings.Contains(detail, tt.detail) {
				t.Errorf("detail %q does not contain %q", detail, tt.detail)
			}
			if strings.Contains(fmt.Sprint(entry.Detail), format.ErrInvalid.Error()) {
				t.Errorf("detail %q repeats the sentinel", entry.Detail)
			}
		})
	}
	if refs := e.prov.PushReferences(); len(refs) != 0 {
		t.Errorf("a refused manifest reached the backend: %+v", refs)
	}
}

// TestManifestHookByDigest: a push by digest is tagged from the annotations, and the answer
// says where; a subject is answered with OCI-Subject.
func TestManifestHookByDigest(t *testing.T) {
	e := newEnv(t, envOptions{})
	f := e.newRawFile("docs/by-digest.txt", "pushed by digest")
	subject := digest.FromString("a manifest this one refers to")
	f.m.Subject = &ocispec.Descriptor{MediaType: ocispecManifest, Digest: subject, Size: 42}
	body, d := f.body(t)
	rr := e.put(f, d.String(), body)
	wantStatus(t, "push by digest", rr, http.StatusCreated)
	wantHeader(t, "push by digest", rr, "Location", "/v2/raw/assets/docs/by-digest.txt/manifests/"+f.tag())
	wantHeader(t, "push by digest", rr, "Docker-Content-Digest", d.String())
	wantHeader(t, "push by digest", rr, "OCI-Subject", subject.String())
	if refs := e.prov.PushReferences(); len(refs) != 1 || refs[0].Tag != f.tag() || refs[0].Digest != d {
		t.Errorf("PushReferences = %+v", refs)
	}
	tags := e.do(e.h, http.MethodGet, "/v2/raw/assets/docs/by-digest.txt/tags/list", nil)
	if want := `{"name":"raw/assets/docs/by-digest.txt","tags":["` + f.tag() + `"]}` + "\n"; tags.Body.String() != want {
		t.Errorf("tags/list = %s, want %s", tags.Body, want)
	}
}

// TestManifestHookSpelling is the lead's note on #25 from #97, and #244: a raw path pushed in
// another spelling passes Validate and is refused under ingest's claims, under another tag
// (other bytes) and under the first spelling's own tag (the same bytes): 409 with the
// conflict's message, which names both spellings, and the manifest is not pushed.
func TestManifestHookSpelling(t *testing.T) {
	e := newEnv(t, envOptions{})
	first := e.newRawFile("docs/readme.md", "first spelling")
	for _, k := range []string{profile.AnnotationName, profile.AnnotationRawPath} {
		first.m.Annotations[k] = "Docs/Readme.MD"
	}
	first.m.Layers[0].Annotations[ocispec.AnnotationTitle] = "Docs/Readme.MD"
	body, d := first.body(t)
	wantStatus(t, "first spelling", e.put(first, first.tag(), body), http.StatusCreated)
	for _, content := range []string{"second spelling, other bytes", "first spelling"} {
		second := e.newRawFile("docs/readme.md", content)
		body, _ = second.body(t)
		entry := wantEntry(t, "second spelling", e.put(second, second.tag(), body), http.StatusConflict, codeTagInvalid)
		if !strings.Contains(entry.Message, `"docs/readme.md" exists as "Docs/Readme.MD"`) || entry.Detail != nil {
			t.Errorf("message %q, detail %#v: want both spellings and no detail", entry.Message, entry.Detail)
		}
	}
	if refs := e.prov.PushReferences(); len(refs) != 1 || refs[0].Digest != d {
		t.Errorf("PushReferences = %+v, want the first spelling's only", refs)
	}
}

// TestManifestHookSpellingMaven is #244 as it was found: after com.example:lib-core 1.2.0,
// Com.Example:lib-core 1.2.0 pushed through /v2 to the same OCI repository and tag is 409 naming
// com.example:lib-core, and the tag keeps the first manifest.
func TestManifestHookSpellingMaven(t *testing.T) {
	e := newEnv(t, envOptions{})
	repo := index.Repo{Format: "maven", Name: "releases", Registry: "local", Prefix: "maven/releases", Kind: "hosted"}
	must(t, e.st.CreateRepo(t.Context(), &repo))
	var first digest.Digest
	for _, group := range []string{"com.example", "Com.Example"} {
		built, body := e.mavenBuilt(repo, group)
		rr := e.do(e.h, http.MethodPut, "/v2/"+built.RepoPath+"/manifests/"+built.Tag, bytes.NewReader(body),
			"Content-Type", ocispecManifest, principalHeader, "alice")
		if first == "" {
			wantStatus(t, group, rr, http.StatusCreated)
			first = digest.FromBytes(body)
			continue
		}
		entry := wantEntry(t, group, rr, http.StatusConflict, codeTagInvalid)
		if !strings.Contains(entry.Message, `"Com.Example:lib-core" exists as "com.example:lib-core"`) {
			t.Errorf("message %q, want both spellings", entry.Message)
		}
	}
	if refs := e.prov.PushReferences(); len(refs) != 1 || refs[0].Digest != first {
		t.Errorf("PushReferences = %+v, want the first spelling's only", refs)
	}
}

// mavenBuilt is lib-core 1.2.0 of group, a jar and its POM, as the maven plugin builds it; its
// blobs are in the fake, as a client pushes them before the manifest, whose bytes it returns.
func (e *env) mavenBuilt(repo index.Repo, group string) (*format.Built, []byte) {
	e.t.Helper()
	ctx := e.t.Context()
	jar, jarInfo := testutil.Jar(testutil.JarOptions{GroupID: group})
	pom, pomInfo := testutil.POM(testutil.POMOptions{GroupID: group})
	var up format.Upload
	for path, b := range map[string][]byte{jarInfo.Path: jar, pomInfo.Path: pom} {
		up.Files = append(up.Files, format.UploadedFile{Name: path, Size: int64(len(b)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }})
	}
	plugin, ok := format.Lookup("maven")
	if !ok {
		e.t.Fatal("the maven plugin is not registered")
	}
	built, err := plugin.Build(ctx, repo, up)
	must(e.t, err)
	for _, blob := range built.Blobs {
		rc, err := blob.Open(ctx)
		must(e.t, err)
		b, err := io.ReadAll(rc)
		must(e.t, err)
		_ = rc.Close()
		e.pushBlob(built.RepoPath, b)
	}
	body, _, err := profile.Encode(built.Manifest)
	must(e.t, err)
	return built, body
}

// TestManifestHookSize: the limit of Options.MaxManifestSize holds on managed and oci repos.
func TestManifestHookSize(t *testing.T) {
	e := newEnv(t, envOptions{maxManifest: 256})
	f := e.newRawFile("docs/large.txt", "a manifest over 256 bytes")
	body, _ := f.body(t)
	if len(body) <= 256 {
		t.Fatalf("the manifest has %d bytes", len(body))
	}
	msg := wantError(t, "managed", e.put(f, f.tag(), body), http.StatusRequestEntityTooLarge, codeManifestInvalid)
	if !strings.Contains(msg, "the limit is 256") {
		t.Errorf("message %q", msg)
	}
	rr := e.do(e.h, http.MethodPut, "/v2/oci/images/large/manifests/v1", bytes.NewReader(body), "Content-Type", ocispecManifest)
	wantError(t, "oci", rr, http.StatusRequestEntityTooLarge, codeManifestInvalid)
}

// TestManifestHookMaxFileSize: a manifest whose file is larger than the repo policy's
// maxFileSize is 413 SIZE_INVALID naming the file and the limit, from ingest, and nothing is
// indexed; at the limit it is stored.
func TestManifestHookMaxFileSize(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.raw.Policy = json.RawMessage(`{"maxFileSize":10}`)
	must(t, e.st.UpdateRepo(t.Context(), &e.raw))
	f := e.newRawFile("docs/large.txt", "eleven byte")
	body, _ := f.body(t)
	msg := wantError(t, "above the limit", e.put(f, f.tag(), body), http.StatusRequestEntityTooLarge, codeSizeInvalid)
	if want := "the file docs/large.txt of 11 bytes is larger than the maxFileSize of raw/assets, 10 bytes"; msg != want {
		t.Errorf("message %q, want %q", msg, want)
	}
	if _, err := e.st.GetPackage(t.Context(), e.raw.ID, "docs/large.txt"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the package of the refused manifest: %v, want none", err)
	}
	f = e.newRawFile("docs/fits.txt", "ten bytes!")
	body, _ = f.body(t)
	if rr := e.put(f, f.tag(), body); rr.Code != http.StatusCreated {
		t.Errorf("a file at the limit: %d %s, want 201", rr.Code, rr.Body)
	}
}

// stubIngester answers every Ingest with err.
type stubIngester struct{ err error }

func (s stubIngester) Ingest(context.Context, index.Repo, string, string, ocispec.Manifest, []byte, auth.Principal) (*ingest.Result, error) {
	return nil, s.err
}

// TestManifestHookErrors is the error table of the hook (ingestError), with the pipeline's
// errors as they come, and the backend failing the blob check.
func TestManifestHookErrors(t *testing.T) {
	existing := digest.FromString("the manifest the tag holds")
	tests := []struct {
		name    string
		err     error
		unknown bool // oras's default artifactType
		status  int
		code    string
		message string // the whole message, or a part when it ends in "…"
		detail  any
	}{
		{name: "invalid", err: fmt.Errorf("%w: %w", ingest.ErrInvalid, errors.Join(errors.New("rule one"), errors.New("rule two"))),
			status: http.StatusBadRequest, code: codeManifestInvalid,
			message: "the manifest is not a valid raw artifact for raw/assets/docs/e.txt", detail: "rule one; rule two"},
		{name: "invalid with oras's default artifactType", err: fmt.Errorf("%w: artifactType", ingest.ErrInvalid), unknown: true,
			status: http.StatusBadRequest, code: codeManifestInvalid,
			message: "the manifest is not a valid raw artifact for raw/assets/docs/e.txt; pass --artifact-type application/vnd.lajstrom.raw.v1", detail: "artifactType"},
		{name: "too large", err: fmt.Errorf("%w: a file of 11 bytes: %w", ingest.ErrInvalid, &http.MaxBytesError{Limit: 10}),
			status: http.StatusRequestEntityTooLarge, code: codeSizeInvalid,
			message: "format: invalid: a file of 11 bytes: http: request body too large"},
		{name: "tag conflict", err: fmt.Errorf("ingest: push: %w", &index.TagConflictError{Tag: "t1", Existing: existing}),
			status: http.StatusConflict, code: codeTagInvalid,
			message: "tag t1 of raw/assets/docs/e.txt is taken by " + existing.String() + ": tags are immutable",
			detail:  map[string]any{"existing": existing.String()}},
		{name: "other conflict", err: fmt.Errorf("ingest: index: %w: the path is held by Docs/E.txt", ingest.ErrConflict),
			status: http.StatusConflict, code: codeTagInvalid, message: "ingest: index: index: conflict: the path is held by Docs/E.txt"},
		{name: "unsupported", err: fmt.Errorf("ingest: no plugin: %w", format.ErrUnsupported),
			status: http.StatusNotImplemented, code: codeUnsupported, message: "ingest: no plugin: format: unsupported"},
		{name: "backend refuses our credentials", err: fmt.Errorf("PUT https://backend.example/v2: %w", provider.ErrUnauthorized),
			status: http.StatusBadGateway, code: codeUnknown, message: "Bad Gateway"},
		{name: "anything else", err: errors.New("ingest: index raw/assets/docs/e.txt:t1 after https://backend.example accepted"),
			status: http.StatusInternalServerError, code: codeUnknown, message: "Internal Server Error"},
		{name: "a not-found from the backend", err: fmt.Errorf("ingest: resolve: %w", provider.ErrNotFound),
			status: http.StatusInternalServerError, code: codeUnknown, message: "Internal Server Error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, envOptions{ingest: stubIngester{tt.err}})
			f := e.newRawFile("docs/e.txt", "error table")
			if tt.unknown {
				f.m.ArtifactType = unknownArtifactType
			}
			body, _ := f.body(t)
			rr := e.put(f, "t1", body)
			entry := wantEntry(t, tt.name, rr, tt.status, tt.code)
			if entry.Message != tt.message {
				t.Errorf("message %q, want %q", entry.Message, tt.message)
			}
			if fmt.Sprint(entry.Detail) != fmt.Sprint(tt.detail) {
				t.Errorf("detail %#v, want %#v", entry.Detail, tt.detail)
			}
			if strings.Contains(rr.Body.String(), "backend.example") {
				t.Errorf("the answer names the backend: %s", rr.Body)
			}
			if logged := strings.Contains(e.logs.String(), "facade: request failed"); logged != (tt.code == codeUnknown) {
				t.Errorf("logged %v, want %v:\n%s", logged, tt.code == codeUnknown, e.logs)
			}
		})
	}

	e := newEnv(t, envOptions{})
	f := e.newRawFile("docs/e.txt", "the backend fails")
	body, _ := f.body(t)
	e.prov.FailExists = errors.New("HEAD https://backend.example/v2: connection reset")
	wantError(t, "the blob check fails", e.put(f, f.tag(), body), http.StatusInternalServerError, codeUnknown)
	e.prov.FailExists = nil

	h := New(Options{Index: e.st, Providers: provider.Map{"local": e.reg}, Tokens: e.tokens, Log: slog.New(slog.NewTextHandler(e.logs, nil))})
	rr := e.do(h, http.MethodPut, "/v2/raw/assets/docs/e.txt/manifests/"+f.tag(), bytes.NewReader(body), "Content-Type", ocispecManifest)
	wantError(t, "no ingest pipeline", rr, http.StatusInternalServerError, codeUnknown)
	if !strings.Contains(e.logs.String(), "Options.Ingest is nil") {
		t.Errorf("the missing pipeline is not logged:\n%s", e.logs)
	}
}

// TestArtifactTypeOf: the hint names the artifactType of the repo's format, which for apt is
// deb's (manifest-profile.md §1), and for a format without a type its own name.
func TestArtifactTypeOf(t *testing.T) {
	for f, want := range map[string]string{
		"raw": "application/vnd.lajstrom.raw.v1", "apt": "application/vnd.lajstrom.deb.v1", "rpm": "application/vnd.lajstrom.rpm.v1",
	} {
		if got := artifactTypeOf(f); got != want {
			t.Errorf("artifactTypeOf(%q) = %q, want %q", f, got, want)
		}
	}
}
