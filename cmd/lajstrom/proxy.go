package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// loadUpstreams returns the clients of the configured upstreams for serve, with their
// credential files read (proxy.LoadUpstreams). An upstream of a format that has no proxy repos
// in this build is a configuration error (docs/configuration.md, upstreams).
func loadUpstreams(c *config.Config, plugins func(string) (format.Plugin, bool), log *slog.Logger) (map[string]*proxy.Upstream,
	map[string]*proxy.OCIUpstream, error) {
	for i, u := range c.Upstreams {
		if u.Format == config.FormatOCI {
			continue // LoadOCIUpstreams's (ADR 0018)
		}
		p, ok := plugins(u.Format)
		if _, proxier := p.(format.Proxier); !ok || !proxier {
			return nil, nil, fmt.Errorf("upstreams[%d].format: %q is not a format of this build that has proxy repos", i, u.Format)
		}
	}
	ups, err := proxy.LoadUpstreams(c.Upstreams, proxy.UpstreamOptions{Log: log})
	if err != nil {
		return nil, nil, err
	}
	ociUps, err := proxy.LoadOCIUpstreams(c.Upstreams, log)
	if err != nil {
		return nil, nil, err
	}
	if n := len(ups) + len(ociUps); n > 0 {
		log.Info("upstreams of proxy repos configured", "upstreams", n, "oci", len(ociUps))
	}
	return ups, ociUps, nil
}

// checkProxyRepos logs an error for each proxy repo whose upstream the configuration does not
// have: it is served as if it were offline, and serve starts anyway (proxy-and-group.md §2.1).
func checkProxyRepos(ctx context.Context, st index.Reader, ups map[string]*proxy.Upstream, ociUps map[string]*proxy.OCIUpstream,
	log *slog.Logger) error {
	repos, err := st.ListRepos(ctx, "")
	if err != nil {
		return err
	}
	for _, r := range repos {
		if r.Kind != index.KindProxy {
			continue
		}
		s, err := proxy.ParseSettings(r.Upstream)
		_, ok := ups[s.Upstream]
		if r.Format == config.FormatOCI {
			_, ok = ociUps[s.Upstream]
		}
		switch {
		case err != nil:
			log.ErrorContext(ctx, "a proxy repo has settings that do not read; it is not served", "repo", r.Format+"/"+r.Name,
				"error", err.Error())
		case !ok:
			log.ErrorContext(ctx, "a proxy repo names an upstream the configuration does not have; it is served as if offline",
				"repo", r.Format+"/"+r.Name, "upstream", s.Upstream)
		}
	}
	return nil
}

// proxySweepInterval is how often serve deletes the negative entries of the proxy repos whose
// negativeTTL is over, with the expired sessions.
const proxySweepInterval = time.Minute

// sweepProxyMeta runs core.Sweep every interval until ctx ends. Nothing depends on it, it keeps
// proxy_meta small; a failure is logged and the next tick tries again.
func sweepProxyMeta(ctx context.Context, core *proxy.Core, log *slog.Logger, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := core.Sweep(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			log.WarnContext(ctx, "expired negative entries of proxy repos not deleted", "error", err.Error())
		case n > 0:
			log.DebugContext(ctx, "expired negative entries of proxy repos deleted", "count", n)
		}
	}
}
