package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Options configures a Core.
type Options struct {
	// Index is the index database; required.
	Index index.Store
	// Ingest is the one write path, through which a fetched artifact is published.
	Ingest format.Publisher
	// Providers resolves a repo's registry, whose maxBlobSize bounds a download.
	Providers provider.Set
	// Upstreams are the configured upstreams by name (LoadUpstreams). A proxy repo whose
	// upstream is not among them is served as if it were offline.
	Upstreams map[string]*Upstream
	// Plugins looks up the plugin of a repo's format; nil means format.Lookup.
	Plugins func(name string) (format.Plugin, bool)
	// Audit records the refused fetches (proxy.reject); nil means the index's audit table.
	Audit audit.Writer
	// ExternalURL is server.externalURL, the base of every link a proxy's document renders.
	ExternalURL string
	// Context is the server's: a fetch runs on a context of its own that ends with it, not with
	// the request that started it; nil means context.Background().
	Context context.Context
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
	// Replica names this process in the fetch claims: host name and process id when empty.
	Replica string
	// ClaimHeartbeat and ClaimStale are the beat of a fetch claim and the age at which a claim
	// that stopped beating is taken over (index-schema.md: 30 s and 2 minutes, the defaults).
	ClaimHeartbeat, ClaimStale time.Duration
	// WaitFirst and WaitLongest are the first and the longest wait of a request whose artifact
	// another replica fetches (proxy-and-group.md §5, step 6: 250 ms, 2 s, the defaults).
	WaitFirst, WaitLongest time.Duration
}

// The defaults of the claims (docs/spec/index-schema.md, Proxy and group repos) and of the
// waits for another replica's fetch (proxy-and-group.md §5, step 6).
const (
	defaultClaimHeartbeat = 30 * time.Second
	defaultClaimStale     = 2 * time.Minute
	defaultWaitFirst      = 250 * time.Millisecond
	defaultWaitLongest    = 2 * time.Second
)

// Core serves the proxy repos of every format whose plugin has the hooks (format.Proxier).
// Its methods may be called concurrently; what must be coordinated across replicas goes
// through the index (proxy_meta, fetch_claims).
type Core struct {
	o Options
}

// New returns a Core with the defaults of Options applied.
func New(o Options) *Core {
	if o.Plugins == nil {
		o.Plugins = format.Lookup
	}
	if o.Context == nil {
		o.Context = context.Background()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Replica == "" {
		host, _ := os.Hostname()
		o.Replica = fmt.Sprintf("%s:%d", host, os.Getpid())
	}
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{
		{&o.ClaimHeartbeat, defaultClaimHeartbeat}, {&o.ClaimStale, defaultClaimStale},
		{&o.WaitFirst, defaultWaitFirst}, {&o.WaitLongest, defaultWaitLongest},
	} {
		if *d.v <= 0 {
			*d.v = d.def
		}
	}
	return &Core{o: o}
}

// proxied is a proxy repo with what serving it takes: its plugin and hooks, its settings, and
// its upstream's client, nil when it is offline or its upstream is not configured (§4.4).
type proxied struct {
	repo     index.Repo
	plugin   format.Plugin
	hooks    format.Proxier
	settings Settings
	up       *Upstream
}

// resolve returns the proxy repo repo is, or an error wrapping format.ErrUnsupported for
// another kind or a format without the hooks.
func (c *Core) resolve(repo index.Repo) (*proxied, error) {
	id := repo.Format + "/" + repo.Name
	if repo.Kind != index.KindProxy {
		return nil, fmt.Errorf("%w: proxy: %s is a %s repo", format.ErrUnsupported, id, repo.Kind)
	}
	plugin, ok := c.o.Plugins(repo.Format)
	if !ok {
		return nil, fmt.Errorf("%w: proxy: no plugin of the format of %s", format.ErrUnsupported, id)
	}
	hooks, ok := plugin.(format.Proxier)
	if !ok {
		return nil, fmt.Errorf("%w: proxy: format %s has no proxy repos", format.ErrUnsupported, repo.Format)
	}
	settings, err := ParseSettings(repo.Upstream)
	if err != nil {
		return nil, fmt.Errorf("proxy: the settings of %s: %w", id, err)
	}
	p := &proxied{repo: repo, plugin: plugin, hooks: hooks, settings: settings}
	if up, ok := c.o.Upstreams[settings.Upstream]; ok && up.Format() == repo.Format && !settings.Offline {
		p.up = up
	}
	return p, nil
}

// id is the repo's <format>/<name>.
func (p *proxied) id() string { return p.repo.Format + "/" + p.repo.Name }

// key returns the plugin's key of name, after the allow and deny lists (§3): a name they
// refuse is format.ErrForbidden naming the list and the pattern, and nothing is asked of the
// upstream for it.
func (p *proxied) key(name string) (string, error) {
	key, err := p.hooks.Key(name)
	if err != nil {
		return "", err
	}
	if why, refused := p.settings.Refuses(key); refused {
		return "", fmt.Errorf("%w: %s does not mirror %q: %s", format.ErrForbidden, p.id(), name, why)
	}
	return key, nil
}

// base is the start of every link of a proxy repo's documents: <externalURL>/<format>/<repo>/.
func (c *Core) base(repo index.Repo) string {
	return strings.TrimSuffix(c.o.ExternalURL, "/") + "/" + repo.Format + "/" + repo.Name + "/"
}

// fresh reports whether a proxy_meta row is used as it is at now: its checked_at plus the
// repo's current TTL of its state is in the future (§4.1).
func (p *proxied) fresh(row *index.ProxyMeta, now time.Time) bool {
	ttl := p.settings.MetadataTTL
	if row.State == index.ProxyMetaAbsent {
		ttl = p.settings.NegativeTTL
	}
	return now.Before(row.CheckedAt.Add(ttl))
}

// metaRow returns the proxy_meta row of a name key and kind on r, nil when there is none.
func metaRow(ctx context.Context, r index.Reader, repoID int64, key, kind string) (*index.ProxyMeta, error) {
	row, err := r.GetProxyMeta(ctx, repoID, key, kind)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil
	}
	return row, err
}

// Sweep deletes the absent proxy_meta rows whose negativeTTL is over, of every proxy repo, and
// returns how many it deleted (§4.3); serve calls it every minute with the expired sessions.
// Present rows stay until they are replaced or purged.
func (c *Core) Sweep(ctx context.Context) (int, error) {
	repos, err := c.o.Index.ListRepos(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("proxy: sweep: %w", err)
	}
	now, total := c.o.Now(), 0
	for _, r := range repos {
		if r.Kind != index.KindProxy {
			continue
		}
		s, err := ParseSettings(r.Upstream)
		if err != nil {
			continue // resolve reports it on the repo's next request
		}
		n, err := c.o.Index.ExpireProxyMeta(ctx, r.ID, now.Add(-s.NegativeTTL))
		if err != nil {
			return total, fmt.Errorf("proxy: sweep %s/%s: %w", r.Format, r.Name, err)
		}
		total += n
	}
	return total, nil
}
