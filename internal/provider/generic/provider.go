package generic

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Type is the registries[].type of this provider.
const Type = "generic"

func init() {
	provider.Register(Type, func(cfg config.Registry, log *slog.Logger) (provider.Provider, error) {
		return New(cfg, log)
	})
}

const (
	// pageSize is the page size of the catalog, tag list and referrers requests.
	pageSize = 100
	// maxPages stops a listing after this many pages: distribution's catalog pagination is not
	// reliable, and a registry must not keep Lajstrom paging forever.
	maxPages = 1000
)

var (
	_ provider.Provider   = (*Provider)(nil)
	_ provider.Repository = (*repository)(nil)
)

// Provider is the Provider of one registry that follows the OCI distribution spec, built on
// oras-go's remote.Registry. It is safe for concurrent use.
type Provider struct {
	name     string
	host     string // host:port of the registry URL
	base     string // scheme://host
	prefix   string // the instance prefix, "" or ending in "/"
	override config.CapabilityOverrides
	log      *slog.Logger
	clients  clients
	registry *remote.Registry
	maxPages int // catalog pages; a field so that tests can lower it

	// mu serialises probing; probed is the answer of the first probe that succeeded.
	mu     sync.Mutex
	probed atomic.Pointer[provider.Capabilities]
	// referrers is what the probe learnt about the referrers API, for the repositories that
	// Repository hands out.
	referrers atomic.Int32
	// deleteRefused is set by the first manifest delete that the registry refuses as deletes,
	// not as this one delete (refusesDelete, deniedOne).
	deleteRefused atomic.Bool
}

// Referrers API support as the probe found it.
const (
	referrersUnknown int32 = iota
	referrersSupported
	referrersUnsupported
)

// New returns the Provider for cfg. It does not talk to the registry; Capabilities probes on
// first use. The URL must be the registry's base URL, without a path: the distribution API
// lives at /v2/ of the host. A nil log means slog.Default(); every line carries
// registry=<name>.
func New(cfg config.Registry, log *slog.Logger) (*Provider, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("generic: url must be an absolute http(s) URL")
	}
	if u.User != nil {
		return nil, errors.New("generic: url must not contain credentials")
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("generic: url must not have a path, query or fragment: the API lives at /v2/ of the host")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("generic: timeout must not be negative")
	}
	if log == nil {
		log = slog.Default()
	}
	log = log.With("registry", cfg.Name)
	p := &Provider{
		name:     cfg.Name,
		host:     u.Host,
		base:     u.Scheme + "://" + u.Host,
		prefix:   normalisePrefix(cfg.Prefix),
		override: cfg.Capabilities,
		log:      log,
		clients:  newClients(cfg, u.Host, log),
		maxPages: maxPages,
	}
	reg := &remote.Registry{RepositoryListPageSize: pageSize}
	reg.Reference = registry.Reference{Registry: u.Host}
	if err := reg.Reference.ValidateRegistry(); err != nil {
		return nil, fmt.Errorf("generic: url: %w", err)
	}
	reg.Client = p.clients.follow
	reg.PlainHTTP = u.Scheme == "http"
	reg.TagListPageSize = pageSize
	reg.ReferrerListPageSize = pageSize
	reg.TagListMaxPages = maxPages
	reg.ReferrerListMaxPages = maxPages
	p.registry = reg
	return p, nil
}

// normalisePrefix returns the instance prefix as it is prepended to names: "" or "<prefix>/".
// One leading and one trailing slash are dropped first, so "art", "art/" and "/art/" are alike.
func normalisePrefix(prefix string) string {
	prefix = strings.TrimSuffix(strings.TrimPrefix(prefix, "/"), "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

// Name returns registries[].name.
func (p *Provider) Name() string { return p.name }

// repoName is the OCI repository name of the Lajstrom name: the instance prefix and name.
func (p *Provider) repoName(name string) string { return p.prefix + name }

// Repository returns the repository name, below the instance prefix. It does not talk to the
// registry. The repository translates errors into the sentinels of package provider and, once
// the capabilities are known, uses the referrers API or the tag schema without probing again.
func (p *Provider) Repository(ctx context.Context, name string) (provider.Repository, error) {
	if name == "" {
		return nil, errors.New("generic: repository: empty name")
	}
	r, err := p.registry.Repository(ctx, p.repoName(name))
	if err != nil {
		return nil, translate("repository "+name, err)
	}
	rr, ok := r.(*remote.Repository)
	if !ok {
		return nil, fmt.Errorf("generic: repository %s: oras-go returned %T", name, r)
	}
	if capable, known := p.referrersCapability(); known {
		// Fails only when the state is already set, and it is not: rr is new.
		_ = rr.SetReferrersCapability(capable)
	}
	return &repository{Repository: rr, p: p, name: name}, nil
}

// EnsureRepository does nothing: the registries of this provider create a repository on the
// first push (Capabilities.CreateOnPush).
func (p *Provider) EnsureRepository(context.Context, string) error { return nil }

// DeleteRepository is not supported: the distribution API has no repository delete. Lajstrom
// deletes the manifests of a repo instead.
func (p *Provider) DeleteRepository(_ context.Context, name string) error {
	return fmt.Errorf("generic: delete repository %s: %w", name, provider.ErrUnsupported)
}

// errStopWalk ends a catalog walk early without an error.
var errStopWalk = errors.New("stop the catalog walk")

// ListRepositories walks the registry's catalog (_catalog, with Link pagination) and returns
// the names below the instance prefix that start with prefix, without the instance prefix,
// sorted and without duplicates. The walk stops when a page repeats the previous one or after
// maxPages pages, logged at warn: the result is then what the pages so far listed. It returns
// an error wrapping provider.ErrUnsupported when capabilities.catalog is false, when the probe
// found no catalog, and when the registry answers the catalog with 401, 403, 404 or 405.
func (p *Provider) ListRepositories(ctx context.Context, prefix string) ([]string, error) {
	op := "list repositories " + prefix
	if !p.catalogUsable() {
		return nil, fmt.Errorf("generic: %s: no catalog: %w", op, provider.ErrUnsupported)
	}
	want := p.repoName(prefix)
	found := map[string]struct{}{}
	var prev []string
	pages := 0
	err := p.registry.Repositories(ctx, "", func(page []string) error {
		pages++
		if pages > 1 && slices.Equal(page, prev) {
			p.log.DebugContext(ctx, "catalog page repeats the previous one; stopping", "pages", pages)
			return errStopWalk
		}
		prev = slices.Clone(page)
		for _, n := range page {
			if strings.HasPrefix(n, want) {
				found[strings.TrimPrefix(n, p.prefix)] = struct{}{}
			}
		}
		if pages >= p.maxPages {
			p.log.WarnContext(ctx, "catalog has more pages than Lajstrom reads; the listing is incomplete", "pages", pages)
			return errStopWalk
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		if resp, ok := catalogMissing(err); ok {
			return nil, &backendError{op: op, kind: provider.ErrUnsupported, detail: responseDetail(resp), cause: err}
		}
		return nil, translate(op, err)
	}
	return slices.Sorted(maps.Keys(found)), nil
}
