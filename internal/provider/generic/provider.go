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
	"time"

	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

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
	// spoolUnknown is registries[].uploads.spoolUnknownLength: a chunk of unknown length into a
	// session that holds bytes is spooled to learn its length (spool.go).
	spoolUnknown bool
	// spoolHint is set by the first probe that succeeded when the vendor table hints at
	// spoolUnknownLength and it is off: a streamed chunk the registry refuses says so (hintSpool).
	spoolHint atomic.Bool
	// chunk is the size of the chunks of OpenBlobWriter (blobChunk).
	chunk int
	// bound is the largest PATCH the registry takes, 0 for no bound (chunkBound).
	bound int

	// timeout is registries[].timeout with its default: how long a read waits for a probe
	// (readQuirks).
	timeout time.Duration

	// mu guards flight and failedAt; probed is the answer of the first probe that succeeded.
	mu       sync.Mutex
	flight   *probeFlight // the probe under way, nil when none
	failedAt time.Time    // when the last probe that the read path ran failed (probeBackoff)
	probed   atomic.Pointer[provider.Capabilities]
	// referrers is what the probe learnt about the referrers API, for the repositories that
	// Repository hands out.
	referrers atomic.Int32
	// deleteRefused is set by the first manifest delete that the registry refuses as deletes,
	// not as this one delete (refusesDelete, deniedOne).
	deleteRefused atomic.Bool
	// quirks are the vendor's, set by the first probe that succeeded; none before it.
	quirks atomic.Pointer[quirks]
	// statusConv is how the registry's upload status reads, measured on the first status that
	// disagreed with its session (statusOffset); statusMu serialises the measurement.
	statusMu   sync.Mutex
	statusConv atomic.Int32
}

// quirk returns the vendor's quirks, none before the probe. A request that depends on them
// asks readQuirks, which probes first.
func (p *Provider) quirk() quirks {
	if q := p.quirks.Load(); q != nil {
		return *q
	}
	return quirks{}
}

// readQuirks returns the vendor's quirks for one request of the read path, which does not call
// Capabilities: OpenBlob, StatBlob and a blob's Exists. Before the first probe that succeeded,
// it probes, or waits for the probe under way, at most registries[].timeout, so that the
// workarounds apply from the provider's first request on, also when the probe at start found
// the registry down (#797). The caller asks once and keeps the answer for the request.
//
// A ping the registry did not answer (provider.ErrUnavailable: no answer, refused, 503, the
// circuit open) is the error, and the read sends nothing more: it costs what the read's own
// request would have, in time and toward the circuit breaker, and the next read probes again.
// Any other failure, or a probe that does not end in time, leaves the request without the
// quirks, as before #797, and the read path does not probe again for probeBackoff.
func (p *Provider) readQuirks(ctx context.Context) (quirks, error) {
	if q := p.quirks.Load(); q != nil {
		return *q, nil
	}
	_, unanswered, err := p.probeShared(ctx, p.timeout, true)
	switch {
	case unanswered:
		return quirks{}, err
	case err != nil && !errors.Is(err, errProbeBackoff):
		p.log.DebugContext(ctx, "the probe before a blob read failed; the read goes without the vendor's workarounds", "error", err.Error())
	}
	return p.quirk(), nil
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
	return NewWithOptions(cfg, log, Options{})
}

// Options are what a provider type built on this one changes in New: internal/provider/ecr
// keeps the data path of this package and brings its own credentials. The zero value is New.
type Options struct {
	// Credential answers the registry's challenges in place of registries[].auth, for a
	// credential that expires and is replaced, such as ECR's authorization token. Once the
	// registry asked for Basic, every request asks Credential again instead of reusing what
	// the first challenge got, so a replaced credential is sent from the next request on, not
	// after a 401. A Credential that fails fails the request with provider.ErrUnauthorized.
	Credential auth.CredentialFunc
}

// NewWithOptions is New with the changes of o.
func NewWithOptions(cfg config.Registry, log *slog.Logger, o Options) (*Provider, error) {
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
		clients:  newClients(cfg, u, log, o.Credential),
		maxPages: maxPages,
		timeout:  timeoutOf(cfg),

		spoolUnknown: cfg.Uploads.SpoolUnknownLength,
		chunk:        blobChunk(u.Host, cfg.Uploads.ChunkSize),
		bound:        chunkBound(u.Host, cfg.Uploads.ChunkSize),
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
		return nil, p.translate("repository "+name, err)
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

// ListRepositories walks the registry's catalog (_catalog, with Link pagination, with the
// credentials even when the registry does not challenge it: catalogClient) and returns
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
	err := p.catalogRegistry().Repositories(ctx, "", func(page []string) error {
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
		return nil, p.translate(op, err)
	}
	return slices.Sorted(maps.Keys(found)), nil
}
