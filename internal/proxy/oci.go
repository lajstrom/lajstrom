package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// The proxy_meta kinds of an oci proxy repo, whose rows are named by the path, but for
// oci.holders, which is the repo's (docs/spec/proxy-and-group.md §11.3, index-schema.md).
const (
	kindOCITag       = "oci.tag:"      // + tag: what the upstream's tag names (ociTagRow)
	kindOCIManifest  = "oci.manifest:" // + digest: a manifest's or an index's exact bytes
	kindOCIRef       = "oci.ref:"      // + digest: what lists a blob or a child (ociRefRow)
	kindOCIHeld      = "oci.held:"     // + digest: the backend holds it
	kindOCIFixedTags = "oci.fixed-tags"
	kindOCIHolders   = "oci.holders:" // + digest, named "", the repo's: where the backend holds a blob (ociHoldersRow)
)

// ociFormat is the format of an oci proxy repo, which has no plugin.
const ociFormat = config.FormatOCI

// The bounds of the Retry-After of an upstream's 429 (§11.7).
const (
	ociRetryAfterMost = time.Minute
	ociRetryAfterNone = 5 * time.Second
)

// OCIUpstream is an upstream of format oci (ADR 0018): the registry's client, which Lajstrom
// only reads, and what serving it needs. Its errors name it by its name.
type OCIUpstream struct {
	name      string
	client    provider.Provider
	dockerHub bool
	retries   int // upstreams[].retries: the resumes in a row of a blob's body that get no byte
}

// OCIUpstreamOption sets what an OCIUpstream takes from its configuration.
type OCIUpstreamOption func(*OCIUpstream)

// OCIRetries is upstreams[].retries: how many resumes in a row of a blob's body may get no
// byte (§11.5, #885); 0 means config.DefaultUpstreamRetries, a negative value none.
func OCIRetries(n int) OCIUpstreamOption {
	return func(u *OCIUpstream) {
		switch {
		case n == 0:
			u.retries = config.DefaultUpstreamRetries
		case n < 0:
			u.retries = 0
		default:
			u.retries = n
		}
	}
}

// NewOCIUpstream returns the upstream name served by client, a read-only provider of the
// registry (generic.NewUpstream); dockerHub turns the library/ rule on (§11.7). Without opts,
// its retries are config.DefaultUpstreamRetries.
func NewOCIUpstream(name string, client provider.Provider, dockerHub bool, opts ...OCIUpstreamOption) *OCIUpstream {
	u := &OCIUpstream{name: name, client: client, dockerHub: dockerHub, retries: config.DefaultUpstreamRetries}
	for _, o := range opts {
		o(u)
	}
	return u
}

// Name is the upstream's name in the configuration.
func (u *OCIUpstream) Name() string { return u.name }

// LoadOCIUpstreams returns the clients of the configured upstreams of format oci by name, with
// their credentials read as LoadUpstreams reads them: a password from passwordFile or, resolved
// by config.Load, from passwordEnv, or a token from tokenFile (§11.1). LoadUpstreams leaves those
// upstreams out.
func LoadOCIUpstreams(cfg []config.Upstream, log *slog.Logger) (map[string]*OCIUpstream, error) {
	if log == nil {
		log = slog.Default()
	}
	out := map[string]*OCIUpstream{}
	for i, c := range cfg {
		if c.Format != ociFormat {
			continue
		}
		key := fmt.Sprintf("upstreams[%d]", i)
		cred, err := ociCredential(key, c.Auth, log)
		if err != nil {
			return nil, err
		}
		if c.Insecure() {
			log.Warn("an upstream is reached over plain http: what it serves is not authenticated", "upstream", c.Name)
		}
		client, err := generic.NewUpstream(c, cred, log)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		out[c.Name] = NewOCIUpstream(c.Name, client, c.IsDockerHub(), OCIRetries(c.Retries))
	}
	return out, nil
}

// ociCredential reads the credentials of an oci upstream.
func ociCredential(key string, a config.UpstreamAuth, log *slog.Logger) (generic.Credential, error) {
	switch {
	case a.TokenFile != "":
		token, err := readSecret(key+".auth.tokenFile", a.TokenFile, log)
		return generic.Credential{Token: token}, err
	case a.PasswordFile != "":
		password, err := readSecret(key+".auth.passwordFile", a.PasswordFile, log)
		return generic.Credential{Username: a.Username, Password: password}, err
	case a.PasswordEnv != "":
		return generic.Credential{Username: a.Username, Password: a.Password}, nil
	}
	return generic.Credential{}, nil
}

// ociProxied is an oci proxy repo with what serving one path of it takes: its settings, its
// upstream (nil when offline or not configured), the path as the upstream knows it, and the
// backend's repository of the path.
type ociProxied struct {
	repo     index.Repo
	settings Settings
	up       *OCIUpstream
	path     string
	name     string // the OCI name in the backend: the repo's base and the path
	prov     provider.Provider
	rr       provider.Repository
}

// id is the repo's <format>/<name>.
func (p *ociProxied) id() string { return p.repo.Format + "/" + p.repo.Name }

// upstreamName is the name of the repo's upstream, for logs, metrics and errors.
func (p *ociProxied) upstreamName() string { return p.settings.Upstream }

// OCIPath returns the path under which the oci proxy repo repo keeps what a client names path:
// library/<path> for a path of one component when its upstream is Docker Hub, the path
// otherwise (§11.7). A repo whose settings do not read, or whose upstream is not configured, keeps
// the path as it is.
func (c *Core) OCIPath(repo index.Repo, path string) string {
	if repo.Format != ociFormat || repo.Kind != index.KindProxy || strings.Contains(path, "/") {
		return path
	}
	s, err := ParseSettings(repo.Upstream)
	if err != nil {
		return path
	}
	if up, ok := c.o.OCIUpstreams[s.Upstream]; ok && up.dockerHub {
		return "library/" + path
	}
	return path
}

// resolveOCI returns the oci proxy repo repo at path, after the allow and deny lists (403) and
// the backend's limits on the OCI name (400), before anything is asked of the upstream (§11.2).
func (c *Core) resolveOCI(ctx context.Context, repo index.Repo, path string) (*ociProxied, error) {
	id := repo.Format + "/" + repo.Name
	if repo.Format != ociFormat || repo.Kind != index.KindProxy {
		return nil, fmt.Errorf("%w: proxy: %s is not an oci proxy repo", format.ErrUnsupported, id)
	}
	settings, err := ParseSettings(repo.Upstream)
	if err != nil {
		return nil, fmt.Errorf("proxy: the settings of %s: %w", id, err)
	}
	if why, refused := settings.Refuses(path); refused {
		return nil, fmt.Errorf("%w: %s does not mirror %q: %s", format.ErrForbidden, id, path, why)
	}
	p := &ociProxied{repo: repo, settings: settings, path: path, name: ociName(repo, path)}
	if up, ok := c.o.OCIUpstreams[settings.Upstream]; ok && !settings.Offline {
		p.up = up
	}
	prov, ok := c.o.Providers.Provider(repo.Registry)
	if !ok || prov == nil {
		return nil, fmt.Errorf("proxy: repo %s: registry %q is not configured", id, repo.Registry)
	}
	if caps, err := prov.Capabilities(ctx); err == nil {
		if err := caps.CheckName(p.name); err != nil {
			return nil, fmt.Errorf("%w: %w", format.ErrInvalid, err)
		}
	}
	if p.rr, err = prov.Repository(ctx, p.name); err != nil {
		return nil, fmt.Errorf("proxy: repository %s: %w", p.name, err)
	}
	p.prov = prov
	return p, nil
}

// ociName is the OCI repository of path in repo, as ingest names it: the repo's prefix, or
// <format>/<repo>, and the path.
func ociName(repo index.Repo, path string) string {
	base := strings.Trim(repo.Prefix, "/")
	if base == "" {
		base = repo.Format + "/" + repo.Name
	}
	return base + "/" + path
}

// fresh reports whether a row is used as it is at now: its checked_at plus the repo's TTL of
// its state is in the future (§4.1).
func (p *ociProxied) fresh(row *index.ProxyMeta, now time.Time) bool {
	ttl := p.settings.MetadataTTL
	if row.State == index.ProxyMetaAbsent {
		ttl = p.settings.NegativeTTL
	}
	return now.Before(row.CheckedAt.Add(ttl))
}

// resolve asks the upstream what ref names at path, with a HEAD, which Docker Hub does not count.
func (u *OCIUpstream) resolve(ctx context.Context, path, ref string) (ocispec.Descriptor, error) {
	rr, err := u.client.Repository(ctx, path)
	if err != nil {
		return ocispec.Descriptor{}, u.classify(ctx, err)
	}
	desc, err := rr.Resolve(ctx, ref)
	return desc, u.classify(ctx, err)
}

// classify maps an error of the upstream's client to what a proxy answers (§6, §11.7): a
// repository, manifest or blob it does not have is errMissing; its 429 is a
// *format.UnavailableError with its Retry-After, at most a minute, 5 s without one; a timeout is
// format.ErrGatewayTimeout; refused credentials, a refused realm or redirect, and every other
// failure are format.ErrBadGateway. The end of ctx stays as it is.
func (u *OCIUpstream) classify(ctx context.Context, err error) error {
	var (
		rej *provider.RejectedError
		ne  net.Error
	)
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, provider.ErrNotFound):
		return errMissing
	case errors.As(err, &rej) && rej.Status == 429:
		wait := rej.RetryAfter
		if wait <= 0 {
			wait = ociRetryAfterNone
		}
		return fmt.Errorf("upstream %s answered 429: %w", u.name, &format.UnavailableError{RetryAfter: min(wait, ociRetryAfterMost)})
	case errors.As(err, &ne) && ne.Timeout():
		return fmt.Errorf("%w: upstream %s did not answer in time", format.ErrGatewayTimeout, u.name)
	}
	return fmt.Errorf("%w: upstream %s: %s", format.ErrBadGateway, u.name, err.Error())
}

// upstreamFailed reports whether err is the upstream's failure, after which what is cached or
// held is served (§6): not a name it does not have, and not the end of the caller's context.
func upstreamFailed(err error) bool {
	return err != nil && !errors.Is(err, index.ErrNotFound) && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded)
}

// ociManifestUnknown is the error of a manifest or a tag that neither the upstream nor the repo
// has.
func ociManifestUnknown(p *ociProxied, ref string) error {
	return fmt.Errorf("%w: proxy: %s: upstream %s does not have %s:%s", index.ErrNotFound, p.id(), p.upstreamName(), p.path, ref)
}

// metaKey is a row's kind for a digest.
func metaKey(kind string, d digest.Digest) string { return kind + d.String() }
