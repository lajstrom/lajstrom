package harbor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// Type is the registries[].type of this provider.
const Type = "harbor"

func init() {
	provider.Register(Type, func(cfg config.Registry, log *slog.Logger) (provider.Provider, error) {
		return New(cfg, log)
	})
}

// maxNameLen is the most bytes of a repository name that Harbor takes, its project included:
// v2.15.4 opens an upload session in a name of 245 bytes and answers one of 246 with 500
// (docs/registries/harbor.md, the probe of 2026-11-14). The generic provider's vendor table
// says the same, once its probe has told Harbor by its challenge.
const maxNameLen = 245

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.Pinger   = (*Provider)(nil)
)

// Provider is the Provider of one Harbor project. The data path, /v2, is the generic
// provider's; the project, the listing and the repository delete are Harbor's REST API's. It is
// safe for concurrent use.
type Provider struct {
	name          string
	prefix        string // the instance prefix, ending in "/"
	project       string // its first component
	createProject bool
	override      config.CapabilityOverrides // registries[].capabilities
	api           *client
	data          *generic.Provider
	now           func() time.Time
	log           *slog.Logger

	// mu guards projectUntil, the time until which EnsureRepository believes that the project
	// exists without asking; missedAt, when a missing project was last logged; and facts, what
	// the first Capabilities that succeeded read.
	mu           sync.Mutex
	projectUntil time.Time
	missedAt     time.Time
	facts        *facts
	// check serialises the reads of the first Capabilities.
	check sync.Mutex
}

// facts are what the REST API said about the project at the first Capabilities that succeeded.
type facts struct {
	// catalog: the account may list the project's repositories.
	catalog bool
	// rules is the number of the project's immutability rules that are not disabled; -1 when
	// the account may not read them.
	rules int
}

// New returns the Provider for cfg. The project is the first component of registries[].prefix,
// which must be set. It does not talk to the registry. A nil log means slog.Default().
func New(cfg config.Registry, log *slog.Logger) (*Provider, error) {
	return newProvider(cfg, log, time.Now)
}

// newProvider is New with the clock.
func newProvider(cfg config.Registry, log *slog.Logger, now func() time.Time) (*Provider, error) {
	if log == nil {
		log = slog.Default()
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix == "" {
		return nil, errors.New("harbor: prefix must be set: its first component is the Harbor project")
	}
	project, _, _ := strings.Cut(prefix, "/")
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("harbor: url must be an absolute http(s) URL")
	}
	data := cfg
	data.Capabilities = capabilities(cfg.Capabilities)
	g, err := generic.New(data, log)
	if err != nil {
		return nil, fmt.Errorf("harbor: %w", err)
	}
	return &Provider{
		name:          cfg.Name,
		prefix:        prefix + "/",
		project:       project,
		createProject: cfg.Harbor.CreateProject,
		override:      cfg.Capabilities,
		api:           newClient(cfg, u.Scheme+"://"+u.Host),
		data:          g,
		now:           now,
		log:           log.With("registry", cfg.Name),
	}, nil
}

// capabilities returns the overrides of the data path: registries[].capabilities, with catalog
// true where they leave it unset. /v2/_catalog is for Harbor's system administrators
// (docs/registries/harbor.md, Catalog and listing), and ListRepositories reads the REST API
// instead: the generic provider asks no catalog, and Capabilities answers what the REST API
// said.
func capabilities(o config.CapabilityOverrides) config.CapabilityOverrides {
	if o.Catalog == nil {
		t := true
		o.Catalog = &t
	}
	return o
}

// Name returns registries[].name.
func (p *Provider) Name() string { return p.name }

// Capabilities are the generic provider's, with what the REST API says where the configuration
// sets nothing: Catalog, whether the account may list the project's repositories, and
// ImmutableTags, whether the project has an immutability rule that is not disabled. MaxNameLength
// is maxNameLen less the length of the instance prefix, whatever the generic provider's probe
// told of the vendor. The first answer that succeeds, the probe at start, checks the project
// (creating it with registries[].harbor.createProject) and reads the rest (readFacts); a
// project that does not exist, and an answer of the REST API that is not the account's
// permissions, fail it, and the next call asks again.
func (p *Provider) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	c, err := p.data.Capabilities(ctx)
	if err != nil {
		return c, err
	}
	f, err := p.readFacts(ctx)
	if err != nil {
		return provider.Capabilities{}, err
	}
	if p.override.Catalog == nil {
		c.Catalog = f.catalog
	}
	if p.override.ImmutableTags == nil {
		c.ImmutableTags = f.rules > 0
	}
	c.MaxNameLength = max(maxNameLen-len(p.prefix), 1)
	return c, nil
}

// Repository returns the data path of the repository name, the generic provider's.
func (p *Provider) Repository(ctx context.Context, name string) (provider.Repository, error) {
	return p.data.Repository(ctx, name)
}

// OpenBlob is the generic provider's.
func (p *Provider) OpenBlob(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (provider.Blob, error) {
	return p.data.OpenBlob(ctx, name, d, rangeHeader, redirect)
}

// StatBlob is the generic provider's.
func (p *Provider) StatBlob(ctx context.Context, name string, d digest.Digest) (provider.Blob, error) {
	return p.data.StatBlob(ctx, name, d)
}

// StatManifest is the generic provider's.
func (p *Provider) StatManifest(ctx context.Context, name string, d digest.Digest) (ocispec.Descriptor, error) {
	return p.data.StatManifest(ctx, name, d)
}

// Ping implements provider.Pinger: GET /v2/, the generic provider's.
func (p *Provider) Ping(ctx context.Context) error { return p.data.Ping(ctx) }
