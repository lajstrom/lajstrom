package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Pipeline is the single write path: every manifest Lajstrom stores goes through it, whether
// it comes from the OCI facade (Ingest) or from a protocol frontend (Upload).
// See docs/spec/layout.md §2 and docs/architecture.md §3 (flow A).
type Pipeline struct {
	providers provider.Set
	index     index.Store
	plugins   func(name string) (format.Plugin, bool)
	audit     audit.Writer
	now       func() time.Time
	log       *slog.Logger
	claimTTL  time.Duration
}

// Options configures a Pipeline.
type Options struct {
	// Providers resolves repo.Registry to the backend registry.
	Providers provider.Set
	// Index is the index database.
	Index index.Store
	// Plugins looks up the plugin of a repo's format; nil means format.Lookup.
	Plugins func(name string) (format.Plugin, bool)
	// Audit records every successful publish; nil means the event goes to the index in the
	// transaction that writes the version.
	Audit audit.Writer
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
	// ClaimTTL is how long a claim on a tag holds off a publish of another digest; an older
	// claim is stale and the next publish takes it over (docs/spec/index-schema.md,
	// Coordination). Zero, or negative, means 10 minutes (ingest.claimTTL).
	ClaimTTL time.Duration
}

// defaultClaimTTL is ClaimTTL when Options leaves it zero.
const defaultClaimTTL = 10 * time.Minute

// New returns a Pipeline with the defaults of Options applied.
func New(o Options) *Pipeline {
	p := &Pipeline{
		providers: o.Providers,
		index:     o.Index,
		plugins:   o.Plugins,
		audit:     o.Audit,
		now:       o.Now,
		log:       o.Log,
		claimTTL:  o.ClaimTTL,
	}
	if p.plugins == nil {
		p.plugins = format.Lookup
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.claimTTL <= 0 {
		p.claimTTL = defaultClaimTTL
	}
	return p
}

var _ format.Publisher = (*Pipeline)(nil)

// Result describes a stored manifest.
type Result struct {
	// Digest is the manifest digest.
	Digest digest.Digest
	// Tag is the tag the manifest is stored under.
	Tag string
	// RepoPath is <format>/<repo>/<package path> without the instance prefix.
	RepoPath string
	// Entry is the index entry the plugin returned.
	Entry format.Entry
	// Existed is true when the tag already held this manifest, or, for Upload, a manifest with
	// the same file layers: the call was an idempotent repeat, nothing was written, and Digest
	// is the manifest the tag holds.
	Existed bool
}

// Ingest stores a manifest that already exists as bytes; the facade's PUT manifest hook calls
// it. pkgPath and tag are where the client pushed: the package path below <format>/<repo>/
// and the tag, both taken from the /v2 URL. raw is the manifest exactly as received and
// defines the digest; m is its parsed form. The steps (docs/spec/manifest-profile.md §5):
//
//  1. check the profile rules that hold for every format: artifactType selects the plugin of
//     repo's format, config is the empty descriptor, the required annotations are present and
//     agree with repo, the package path computed from the annotations (§4.1) equals pkgPath
//     and the tag computed from them (§4.2) equals tag;
//  2. check that every layer exists in the backend (the empty config is pushed when missing);
//  3. run the plugin's Validate with a BlobFetcher backed by the provider, limited to the
//     blobs the manifest names;
//  4. claim the tag in the index (docs/spec/index-schema.md, Coordination): another digest
//     already confirmed under the tag, or a fresh claim of another digest, is ErrConflict;
//     the same digest confirmed is Existed and nothing is written;
//  5. check the tag in the backend (another digest there is ErrConflict), then PUT the
//     manifest with its tag in one request (provider.Repository.PushReference); a failure
//     releases the claim;
//  6. confirm in one index transaction, only after the registry accepted the manifest:
//     package, version and files, the OCI repository, the invalidated index_cache, the audit
//     event, and the claim released. When this fails the manifest stays for reindex to pick
//     up and the error is a plain one (500).
//
// Errors wrap ErrInvalid (steps 1 to 3), ErrConflict (steps 4 and 5, with an
// *index.TagConflictError naming the existing digest when it is known), or neither.
func (p *Pipeline) Ingest(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, principal auth.Principal) (*Result, error) {
	plugin, prov, err := p.lookup(repo)
	if err != nil {
		return nil, err
	}
	t := &target{repo: repo, plugin: plugin, pkgPath: pkgPath, name: ociName(repo, pkgPath)}
	if t.rr, err = prov.Repository(ctx, t.name); err != nil {
		return nil, fmt.Errorf("ingest: repository %s: %w", t.name, err)
	}
	return p.ingest(ctx, &publish{target: t, tag: tag, manifest: m, raw: raw, principal: principal})
}

// Upload is the frontend path: the plugin of repo's format builds the manifest from up
// (Plugin.Build), the pipeline pushes the blobs the backend does not have yet, streaming each
// from its source and checking it against its descriptor on the way, and then stores the
// manifest as Ingest does. It implements format.Publisher.
//
// An upload of the same files under a tag that holds them already is the idempotent repeat,
// whatever org.opencontainers.image.created the new build carries: nothing is written, the
// manifest in the registry stays the first, and Published.Digest is its digest. Other files
// under the tag are ErrConflict.
func (p *Pipeline) Upload(ctx context.Context, repo index.Repo, up format.Upload, principal auth.Principal) (*format.Published, error) {
	plugin, prov, err := p.lookup(repo)
	if err != nil {
		return nil, err
	}
	built, err := plugin.Build(ctx, repo, up)
	if err != nil {
		return nil, buildError(repo, err)
	}
	pkgPath, ok := strings.CutPrefix(built.RepoPath, repoBase(repo)+"/")
	if !ok || pkgPath == "" {
		return nil, fmt.Errorf("ingest: the %s plugin built %q, which is not a package path below %s", repo.Format, built.RepoPath, repoBase(repo))
	}
	if v, err := p.repeat(ctx, repo, built); err != nil {
		return nil, err
	} else if v != nil {
		p.log.InfoContext(ctx, "ingest: the tag already holds these files; nothing written", "repo", repo.Format+"/"+repo.Name,
			"package", built.Entry.Name, "tag", built.Tag, "digest", v.Digest.String(), "principal", principal.String())
		return &format.Published{Digest: v.Digest, Tag: built.Tag, RepoPath: built.RepoPath, Entry: built.Entry}, nil
	}
	raw, _, err := profile.Encode(built.Manifest)
	if err != nil {
		return nil, fmt.Errorf("ingest: %w", err)
	}
	t := &target{repo: repo, plugin: plugin, pkgPath: pkgPath, name: ociName(repo, pkgPath)}
	if err := prov.EnsureRepository(ctx, t.name); err != nil {
		return nil, fmt.Errorf("ingest: ensure repository %s: %w", t.name, err)
	}
	if t.rr, err = prov.Repository(ctx, t.name); err != nil {
		return nil, fmt.Errorf("ingest: repository %s: %w", t.name, err)
	}
	if err := t.pushBlobs(ctx, built); err != nil {
		return nil, err
	}
	res, err := p.ingest(ctx, &publish{target: t, tag: built.Tag, manifest: built.Manifest, raw: raw, principal: principal, repeatOnFiles: true})
	if err != nil {
		return nil, err
	}
	return &format.Published{Digest: res.Digest, Tag: res.Tag, RepoPath: res.RepoPath, Entry: res.Entry}, nil
}

// buildError wraps an error of Plugin.Build: the upload is invalid (400) unless the plugin
// said so already, or said it does not build at all (501).
func buildError(repo index.Repo, err error) error {
	err = fmt.Errorf("ingest: %s plugin: build: %w", repo.Format, err)
	if errors.Is(err, format.ErrUnsupported) {
		return err
	}
	return invalid(err)
}

// target is where a publish goes: the repo, its plugin, and the OCI repository of the package.
type target struct {
	repo    index.Repo
	plugin  format.Plugin
	pkgPath string              // the package path below <format>/<repo>/ (Plugin.EscapeName)
	name    string              // <format>/<repo>/<pkgPath>, the repository as the provider names it
	rr      provider.Repository // the data path to name
}

// publish is one manifest on its way through the pipeline; the steps fill it in.
type publish struct {
	*target
	tag       string
	manifest  ocispec.Manifest
	raw       []byte
	principal auth.Principal

	digest   digest.Digest   // of raw
	parsed   *profile.Parsed // profile.Validate of manifest
	entry    *format.Entry   // Plugin.Validate of manifest
	metadata json.RawMessage // for versions.metadata; nil when absent or too large
	claim    index.TagClaim

	repeatOnFiles bool          // Upload: the same file layers under the tag are the idempotent repeat
	existing      digest.Digest // the manifest the tag holds, when the publish is a repeat of it
}

// lookup returns the plugin of repo's format and the provider of its registry.
func (p *Pipeline) lookup(repo index.Repo) (format.Plugin, provider.Provider, error) {
	plugin, ok := p.plugins(repo.Format)
	if !ok {
		return nil, nil, fmt.Errorf("ingest: repo %s/%s: no plugin for the format: %w", repo.Format, repo.Name, format.ErrUnsupported)
	}
	var prov provider.Provider
	if p.providers != nil {
		prov, ok = p.providers.Provider(repo.Registry)
	}
	if prov == nil || !ok {
		return nil, nil, fmt.Errorf("ingest: repo %s/%s: registry %q is not configured", repo.Format, repo.Name, repo.Registry)
	}
	return plugin, prov, nil
}

// repoBase is the path of repo's packages below the instance prefix: repo.Prefix, which the MVP
// leaves empty or sets to <format>/<name> (index.Repo).
func repoBase(repo index.Repo) string {
	if base := strings.Trim(repo.Prefix, "/"); base != "" {
		return base
	}
	return repo.Format + "/" + repo.Name
}

// ociName is the OCI repository of a package as the provider names it, without the instance
// prefix (docs/spec/manifest-profile.md §4.1).
func ociName(repo index.Repo, pkgPath string) string { return repoBase(repo) + "/" + pkgPath }

// ingest runs the steps of Ingest on pub, whose target, tag, manifest, raw bytes and principal
// are set.
func (p *Pipeline) ingest(ctx context.Context, pub *publish) (*Result, error) {
	pub.digest = digest.FromBytes(pub.raw)
	if err := p.validate(ctx, pub); err != nil {
		return nil, err
	}
	existed, err := p.claimTag(ctx, pub)
	if err != nil {
		return nil, err
	}
	if !existed {
		if err := p.push(ctx, pub); err != nil {
			return nil, err
		}
		if existed, err = p.confirm(ctx, pub); err != nil {
			return nil, err
		}
	}
	msg, d := "ingest: published", pub.digest
	if existed {
		msg = "ingest: the tag already holds this manifest; nothing written"
	}
	if pub.existing != "" {
		msg, d = "ingest: the tag already holds these files; nothing written", pub.existing
	}
	p.log.InfoContext(ctx, msg, "repo", pub.repo.Format+"/"+pub.repo.Name, "package", pub.parsed.Annotations.Name,
		"tag", pub.tag, "digest", d.String(), "principal", pub.principal.String())
	return &Result{Digest: d, Tag: pub.tag, RepoPath: pub.name, Entry: *pub.entry, Existed: existed}, nil
}
