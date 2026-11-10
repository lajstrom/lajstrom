package probe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

const (
	// DefaultMaxBlobSize is the largest blob a run pushes unless Options says otherwise.
	DefaultMaxBlobSize = 8 << 20
	// DefaultMaxManifestSize bounds the manifests a run pushes unless Options says otherwise:
	// one byte above 4 MiB, the limit of distribution, Zot and Harbor.
	DefaultMaxManifestSize = 4<<20 + 1
)

// Options are the choices of one run.
type Options struct {
	// Scratch is the name prefix, below the instance prefix, that the run works in; it must not
	// hold anything yet. Empty: lajstrom-probe-<8 random hex digits>.
	Scratch string
	// MaxBlobSize is the largest blob the run pushes; 0 is DefaultMaxBlobSize.
	MaxBlobSize int64
	// MaxManifestSize bounds the manifests of the size ladder; 0 is DefaultMaxManifestSize.
	MaxManifestSize int64
	// Log receives what the generic provider logs besides its requests; nil is slog.Default().
	Log *slog.Logger
}

// ErrNotEmpty is a scratch prefix that already holds something: the run refuses it.
var ErrNotEmpty = errors.New("probe: the scratch prefix is not empty")

// scratchName is the grammar of a scratch prefix: OCI name components, lower case.
var scratchName = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)

// run is the state of one run against one registry.
type run struct {
	cfg     config.Registry
	opts    Options
	c       *client
	tape    *tape
	report  *Report
	scratch string // the full name prefix the run works in, ending in "/"
	rel     string // the same below the instance prefix, for the generic provider
	id      string // random, in the manifests' notes, so that no two runs push the same digest
	// based holds the repositories the empty blob was pushed to.
	based map[string]bool
	// manifests and blobs are what the run pushed, in order, for the cleanup.
	manifests []pushed
	blobs     []pushed
	// found are facts one cell measures and another uses.
	found facts
}

// pushed is a manifest or a blob the run pushed: its repository (below the scratch prefix) and
// digest.
type pushed struct {
	repo   string
	digest digest.Digest
}

// facts are what one cell found that another one needs.
type facts struct {
	catalog        bool // _catalog answers a catalog
	referrersAPI   bool // the referrers API lists the referrers; meaningful when referrersKnown
	referrersKnown bool
	tagsStay       bool // a manifest deleted by digest leaves its tags
	deleteRefused  bool // a manifest delete by digest is refused as deletes
	repointRefused bool // pushing another manifest under an existing tag is refused
	profileRefused bool
}

// Run measures the row of the registry cfg (registries[].type generic) and deletes what it
// pushed. It fails before pushing anything when the generic provider's probe fails (the
// registry does not answer, the credentials are refused, a token realm on another host) and
// when the scratch prefix is not empty (ErrNotEmpty); a cell it could not measure says why.
func Run(ctx context.Context, cfg config.Registry, o Options) (*Report, error) {
	if cfg.Type != "" && cfg.Type != generic.Type {
		return nil, fmt.Errorf("probe: registry %q is of type %s: only type generic is probed", cfg.Name, cfg.Type)
	}
	if o.MaxBlobSize <= 0 {
		o.MaxBlobSize = DefaultMaxBlobSize
	}
	if o.MaxManifestSize <= 0 {
		o.MaxManifestSize = DefaultMaxManifestSize
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	r, err := newRun(cfg, o)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, tapeKey{}, r.tape)
	if err := r.providerProbe(ctx); err != nil {
		return nil, err
	}
	if err := r.checkScratch(ctx); err != nil {
		return nil, err
	}
	r.measureAll(ctx)
	return r.report, nil
}

// newRun prepares a run: the client, the report and the scratch prefix.
func newRun(cfg config.Registry, o Options) (*run, error) {
	base, err := url.Parse(cfg.URL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("probe: registry %q: url must be an absolute http(s) URL", cfg.Name)
	}
	base = &url.URL{Scheme: base.Scheme, Host: base.Host}
	id := randomHex(4)
	name := o.Scratch
	if name == "" {
		name = "lajstrom-probe-" + id
	}
	name = strings.Trim(name, "/")
	if !scratchName.MatchString(name) {
		return nil, fmt.Errorf("probe: scratch prefix %q is not a repository name of lower-case components", name)
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	r := &run{
		cfg:     cfg,
		opts:    o,
		c:       newClient(cfg, base),
		tape:    &tape{base: base},
		report:  newReport(),
		scratch: prefix + name + "/",
		rel:     name + "/",
		id:      id,
		based:   map[string]bool{},
	}
	r.report.Registry, r.report.URL, r.report.Prefix = cfg.Name, base.String(), prefix
	r.report.Scratch = r.scratch
	r.report.Date = time.Now().UTC().Format(time.DateOnly)
	return r, nil
}

// randomHex returns n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// providerProbe asks the generic provider for its capabilities, as serve does: the row "What
// the probe finds".
func (r *run) providerProbe(ctx context.Context) error {
	c := r.in(ctx, "probeFinds")
	p, err := generic.New(r.cfg, slog.New(&providerLog{next: r.opts.Log.Handler(), vendor: &r.report.Vendor}))
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	caps, err := p.Capabilities(c)
	if err != nil {
		return fmt.Errorf("probe: the provider's probe: %w", err)
	}
	r.report.Capabilities = capabilitiesOf(caps)
	r.set("probeFinds", fmt.Sprintf("`referrers=%t catalog=%t`", caps.Referrers, caps.Catalog))
	return nil
}

// measureAll measures the cells in the README's order, then cleans up. A measurement that fails
// says why in each of its cells that it did not measure.
func (r *run) measureAll(ctx context.Context) {
	groups := []struct {
		keys []string
		fn   func(context.Context) error
	}{
		{[]string{"createOnPush", "where"}, r.measureCreate},
		{[]string{"referrers", "ociSubject"}, r.measureReferrers},
		{[]string{"catalog"}, r.measureCatalog},
		{[]string{"tags"}, r.measureTags},
		{[]string{"deleteDigest", "deleteTag"}, r.measureDeletes},
		{[]string{"immutableTags"}, r.measureImmutable},
		{[]string{"blobRedirect", "range"}, r.measureBlobReads},
		{[]string{"largestBlob", "chunkSize"}, r.measureBlobSizes},
		{[]string{"manifestSize"}, r.measureManifestSize},
		{[]string{"profile"}, r.measureProfile},
	}
	for _, g := range groups {
		if err := g.fn(r.in(ctx, g.keys[0])); err != nil {
			for _, k := range g.keys {
				r.failed(k, err)
			}
		}
	}
	r.cleanup(r.in(ctx, "deleteRepository"))
	if n := r.tape.tooMany; n > 0 {
		r.set("rateLimits", fmt.Sprintf("**429** seen %d times during the probe", n))
	}
	r.set("overrides", overridesVerdict(r.report.Capabilities, r.found))
}

// overridesVerdict is the cell "Overrides needed": where the provider's probe and the
// measurements disagree, the registries[].capabilities that set it right.
func overridesVerdict(caps Capabilities, f facts) string {
	var o []string
	if f.referrersKnown && caps.Referrers != f.referrersAPI {
		o = append(o, fmt.Sprintf("`referrers: %t`", f.referrersAPI))
	}
	if caps.Catalog && !f.catalog {
		o = append(o, "`catalog: false`")
	}
	switch {
	case f.tagsStay:
		o = append(o, "`deleteManifest: false`, or a delete leaves the tags")
	case f.deleteRefused && caps.DeleteManifest:
		o = append(o, "`deleteManifest: false` saves the first refused delete")
	}
	if f.repointRefused && !caps.ImmutableTags {
		o = append(o, "`immutableTags: true`")
	}
	if f.profileRefused {
		o = append(o, "no override helps: **the registry refuses Lajstrom's manifests**")
	}
	if len(o) == 0 {
		return "none"
	}
	return strings.Join(o, "; ")
}

// in makes key the cell that the exchanges of ctx's requests go to.
func (r *run) in(ctx context.Context, key string) context.Context {
	r.tape.mu.Lock()
	r.tape.cell = r.report.Cell(key)
	r.tape.mu.Unlock()
	return ctx
}

// set gives the cell of key its measured value.
func (r *run) set(key, value string) {
	c := r.report.Cell(key)
	c.Value, c.Measured = value, true
}

// failed says in the cell of key, unless it has a value, why it could not be measured.
func (r *run) failed(key string, err error) {
	if c := r.report.Cell(key); !c.Measured {
		c.Value = "*not measured*: " + err.Error()
	}
}

// name is the full repository name of repo, below the scratch prefix.
func (r *run) name(repo string) string { return r.scratch + repo }

// path is the path of the distribution API below repo: path(repo, "tags/list").
func (r *run) path(repo, rest string) string { return "/v2/" + r.name(repo) + "/" + rest }

// get sends a GET.
func (r *run) get(ctx context.Context, target string, header http.Header) (answer, error) {
	return r.c.do(ctx, request{method: http.MethodGet, target: target, header: header})
}

// send sends a request without a body.
func (r *run) send(ctx context.Context, method, target string) (answer, error) {
	return r.c.do(ctx, request{method: method, target: target})
}

// startUpload opens an upload session in repo: POST blobs/uploads/.
func (r *run) startUpload(ctx context.Context, repo string) (answer, error) {
	return r.send(ctx, http.MethodPost, r.path(repo, "blobs/uploads/"))
}

// session opens an upload session in repo and returns its location.
func (r *run) session(ctx context.Context, repo string) (*url.URL, error) {
	a, err := r.startUpload(ctx, repo)
	if err != nil {
		return nil, err
	}
	if !a.ok(http.StatusAccepted) || a.location() == nil {
		return nil, fmt.Errorf("the upload session: %s", a.refusal())
	}
	return a.location(), nil
}

// withDigest is the session URL u with the query parameter digest set.
func withDigest(u *url.URL, d digest.Digest) string {
	v := *u
	q := v.Query()
	q.Set("digest", d.String())
	v.RawQuery = q.Encode()
	return v.String()
}

// pushBlob pushes data to repo with a POST and a monolithic PUT, as oras-go does, and keeps it
// for the cleanup when the registry took it. It returns the last answer.
func (r *run) pushBlob(ctx context.Context, repo string, data []byte) (digest.Digest, answer, error) {
	d := digest.FromBytes(data)
	a, err := r.startUpload(ctx, repo)
	if err != nil || !a.ok(http.StatusAccepted) || a.location() == nil {
		return d, a, err
	}
	a, err = r.c.do(ctx, request{
		method: http.MethodPut, target: withDigest(a.location(), d), body: data,
		header: http.Header{"Content-Type": {"application/octet-stream"}},
	})
	if err == nil && a.ok(http.StatusCreated) {
		r.blobs = append(r.blobs, pushed{repo, d})
	}
	return d, a, err
}

// ensureBase pushes the empty blob {} to repo once: the config and the layer of the probe's
// manifests.
func (r *run) ensureBase(ctx context.Context, repo string) error {
	if r.based[repo] {
		return nil
	}
	_, a, err := r.pushBlob(ctx, repo, []byte("{}"))
	if err != nil {
		return err
	}
	if !a.ok(http.StatusCreated) {
		return fmt.Errorf("push of the empty blob to %s: %s", r.name(repo), a.refusal())
	}
	r.based[repo] = true
	return nil
}

// manifest is a manifest's bytes, media type and digest.
type manifest struct {
	mediaType string
	body      []byte
	digest    digest.Digest
}

// desc is the descriptor of m.
func (m manifest) desc() ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: m.mediaType, Digest: m.digest, Size: int64(len(m.body))}
}

// emptyDesc is the empty blob {}, the config and the one layer of the probe's manifests.
var emptyDesc = profile.EmptyConfig

// newManifest is an image manifest of the probe: the empty config, the empty blob as its one
// layer, the note as its description, and an artifactType and subject when they are set.
// It has none of Lajstrom's media types: GitLab with its metadata database refuses those, and
// the profile has a cell of its own.
func (r *run) newManifest(note, artifactType string, subject *ocispec.Descriptor) manifest {
	m := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType,
		Config:       emptyDesc,
		Layers:       []ocispec.Descriptor{emptyDesc},
		Subject:      subject,
		Annotations:  map[string]string{ocispec.AnnotationDescription: "lajstrom registry probe " + r.id + ": " + note},
	}
	return encode(m.MediaType, m)
}

// encode marshals v as a manifest of mediaType.
func encode(mediaType string, v any) manifest {
	b, _ := json.Marshal(v) // a manifest of the probe always marshals
	return manifest{mediaType: mediaType, body: b, digest: digest.FromBytes(b)}
}

// pushManifest pushes m to repo under ref, a tag or m's digest, and keeps it for the cleanup
// when the registry took it.
func (r *run) pushManifest(ctx context.Context, repo, ref string, m manifest) (answer, error) {
	a, err := r.c.do(ctx, request{
		method: http.MethodPut, target: r.path(repo, "manifests/"+ref), body: m.body,
		header: http.Header{"Content-Type": {m.mediaType}},
	})
	if err == nil && a.ok(http.StatusCreated, http.StatusOK) {
		r.manifests = append(r.manifests, pushed{repo, m.digest})
	}
	return a, err
}

// mustPush pushes m under ref and fails unless the registry took it. A registry that drops a
// blob with the last manifest that used it (Gitea) gets the empty blob again.
func (r *run) mustPush(ctx context.Context, repo, ref string, m manifest) error {
	if err := r.ensureBase(ctx, repo); err != nil {
		return err
	}
	a, err := r.pushManifest(ctx, repo, ref, m)
	if err == nil && a.status == http.StatusNotFound && strings.Contains(a.code(), "BLOB_UNKNOWN") {
		r.based[repo] = false
		if err := r.ensureBase(ctx, repo); err != nil {
			return err
		}
		a, err = r.pushManifest(ctx, repo, ref, m)
	}
	if err != nil {
		return err
	}
	if !a.ok(http.StatusCreated, http.StatusOK) {
		return fmt.Errorf("push of a manifest to %s:%s: %s", r.name(repo), ref, a.refusal())
	}
	return nil
}

// manifestAccept is the Accept header of a manifest request: OCI's types, which distribution
// and Harbor want, and Docker's.
var manifestAccept = http.Header{"Accept": {strings.Join([]string{
	ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageIndex,
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")}}

// getManifest asks repo for the manifest ref.
func (r *run) getManifest(ctx context.Context, repo, ref string) (answer, error) {
	return r.get(ctx, r.path(repo, "manifests/"+ref), manifestAccept)
}

// tagList is the body of tags/list.
type tagList struct {
	Tags []string `json:"tags"`
}

// listTags asks for repo's tags with the query (n, last), and returns them with the answer.
func (r *run) listTags(ctx context.Context, target string) ([]string, answer, error) {
	a, err := r.get(ctx, target, nil)
	if err != nil || a.status != http.StatusOK {
		return nil, a, err
	}
	var l tagList
	if err := json.Unmarshal(a.body, &l); err != nil {
		return nil, a, fmt.Errorf("tags/list of %s: %w", target, err)
	}
	return l.Tags, a, nil
}
