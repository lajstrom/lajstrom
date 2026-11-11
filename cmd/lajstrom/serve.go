package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/facade"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/provider"
	_ "github.com/lajstrom/lajstrom/internal/provider/ecr"     // registers registries[].type ecr
	_ "github.com/lajstrom/lajstrom/internal/provider/generic" // registers registries[].type generic
	"github.com/lajstrom/lajstrom/internal/proxy"
	"github.com/lajstrom/lajstrom/internal/server"
	"github.com/lajstrom/lajstrom/internal/version"
)

// registryProbeTimeout bounds the capability probe of each registry at startup.
const registryProbeTimeout = 10 * time.Second

// newServeCmd returns serve: load and validate the configuration (in the root's
// PersistentPreRunE), read the credential files of the upstreams (loadUpstreams), open the
// index database and apply its pending migrations, write this process's row of replicas and beat
// it (startReplica: on SQLite a live row of another replica stops serve here), create the
// repos of the configuration that the control plane does not have, load the master key, build
// a provider for every registry and probe its capabilities, build the server and run it until
// SIGINT or SIGTERM and the drain that follows (server.Run; a second signal stops the process
// at once), then stop the background work and close the database. A server that cannot listen
// returns its error at once.
//
// Migrating on start takes the same lock as lajstrom migrate, so replicas that start together
// apply each migration once. Serve does not start on a schema that requires a newer binary, nor,
// on Postgres, apply a migration that would break the replicas serving on the schema before it
// (index.ApplyMigrations, docs/operations.md, Upgrades). The index is Deps.Index of the format plugins and /-/readyz pings
// it; the registries are Deps.Providers; the ingest pipeline over both, with ingest.claimTTL,
// is Deps.Ingest; the signing keys over the index and the master key (newSigner) are
// Deps.Signer. The bearer-token key is loaded or made right after (newTokenKeys), before the
// server listens, so that a server that cannot load it fails at start (ADR 0010). The repo
// service over the index (api.Repos) runs the config bootstrap before the server listens, and
// serves /api/v1. While no admin exists, the token of auth.bootstrap.tokenFile is made valid
// next (auth.Bootstrap.Arm); without one, a warning says how to make the first admin. The OCI facade over the index and the registries serves /v2/, issues and
// verifies the bearer tokens with that key, and hands the manifests of managed repos to the
// same pipeline; server.maxManifestSize bounds them. The authenticator over the index's users
// reads the HTTP Basic credentials of every request. The RBAC evaluator over the index's
// bindings (auth.RBAC, with auth.anonymousRead) decides on the frontends, at /v2/token, on /v2
// without a bearer and on /api/v1; there is no mode without it (docs/spec/rbac.md §3). The core
// of proxy repos over the index, the pipeline and the upstreams is Deps.Proxy; a proxy repo whose
// upstream is not configured is logged at error, and its expired negative entries are swept
// every minute (ADR 0013).
func newServeCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "serve",
		Short:       "Run the HTTP server",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// The first signal starts the drain; a second one stops the process at once.
			context.AfterFunc(ctx, stop)

			c, v := a.cfg, version.Get()
			metrics.SetBuild(v.Version, v.Commit)
			a.log.Info("starting lajstrom",
				"version", v.Version,
				"commit", v.Commit,
				"config", a.configPath,
				"database.driver", c.Database.Driver,
				"registries", len(c.Registries),
				"repos", len(c.Repos),
			)
			upstreams, err := loadUpstreams(c, format.Lookup, a.log)
			if err != nil {
				return err
			}
			st, err := openStore(ctx, c.Database)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, st.Close()) }()
			// On SQLite this process is the only replica, so no binary of the schema before the run
			// is serving on it: a breaking migration is safe to apply.
			mig, err := st.ApplyMigrations(ctx, index.MigrateOptions{Breaking: c.Database.Driver == index.DialectSQLite})
			if err != nil {
				return migrationHint(err)
			}
			a.log.Info("index migrations applied under the migration lock",
				"database.driver", c.Database.Driver, "applied", mig.Applied, "schema", mig.Version)
			if mig.Version > mig.Known {
				a.log.Warn("the index schema is newer than this binary, and compatible with it",
					"schema", mig.Version, "known", mig.Known)
			}
			rep, err := startReplica(ctx, st, a.log, time.Now)
			if err != nil {
				return err
			}
			// After the background work has ended, before the index closes.
			defer rep.Stop(context.WithoutCancel(ctx))
			repos := newRepos(c, st, a.log)
			created, err := repos.Bootstrap(ctx, c.Repos)
			if err != nil {
				return err
			}
			a.log.Info("repos of the configuration bootstrapped", "configured", len(c.Repos), "created", created)
			boot := &auth.Bootstrap{Index: st, Users: &auth.Users{Index: st, Log: a.log}, TTL: c.Auth.Bootstrap.TTL, Log: a.log}
			if err := boot.Arm(ctx, c.Auth.Bootstrap.TokenFile); err != nil {
				return err
			}
			signer, err := newSigner(ctx, c, a.lookupEnv, st, a.log)
			if err != nil {
				return err
			}
			tokens, err := newTokenKeys(ctx, c, a.lookupEnv, st, a.log)
			if err != nil {
				return err
			}

			providers, err := provider.NewSet(c.Registries, a.log)
			if err != nil {
				return err
			}
			probeRegistries(ctx, providers, a.log)
			health := newRegistryHealth(providers, a.log)
			health.Ping(ctx)
			go health.Run(ctx)
			pipe := ingest.New(ingest.Options{Providers: providers, Index: st, Log: a.log, ClaimTTL: c.Ingest.ClaimTTL})
			runs := &backgroundReindex{ctx: ctx, log: a.log, w: reindex.New(reindex.Options{Index: st, Providers: providers,
				Inspector: pipe, Log: a.log, ClaimTTL: c.Ingest.ClaimTTL, GraceAge: c.Reconcile.GraceAge})}
			// Before the index closes: a run the shutdown cancels finishes as failed. stop ends the
			// background work first, also when serve returns without a signal, because the listener
			// cannot bind or the server cannot be built: else the wait would never end, and serve
			// would hang without printing its error (#372).
			defer func() { stop(); runs.wg.Wait() }()
			if every := c.Reconcile.Interval; every > 0 {
				a.log.Info("reconcile loop on: a reindex of every repo with prune", "interval", every.String())
				runs.wg.Go(func() { runs.w.Reconcile(ctx, every) })
			} else {
				a.log.Info("reconcile loop off (reconcile.interval is 0)")
			}
			runs.wg.Go(func() { expireSessions(ctx, pipe, a.log, sessionExpiryInterval) })
			runs.wg.Go(func() { rep.Run(ctx) })
			if err := checkProxyRepos(ctx, st, upstreams, a.log); err != nil {
				return err
			}
			proxyCore := proxy.New(proxy.Options{Index: st, Ingest: pipe, Providers: providers, Upstreams: upstreams,
				Plugins: lookupIn(servedPlugins(c.Server.ExternalURL, nil)), ExternalURL: c.Server.ExternalURL, Context: ctx, Log: a.log})
			runs.wg.Go(func() { sweepProxyMeta(ctx, proxyCore, a.log, proxySweepInterval) })
			authz := auth.NewRBAC(auth.RBACOptions{Index: st, AnonymousRead: c.Auth.AnonymousRead, Log: a.log})
			deps := format.Deps{Index: st, Providers: providers, Auth: authz, Ingest: pipe, Signer: signer, Sessions: pipe,
				Proxy: proxyCore}
			a.log.Info("authorization by the bindings of the index (RBAC)", "auth.anonymousRead", c.Auth.AnonymousRead)
			v2 := facade.New(facade.Options{Index: st, Providers: providers, Auth: authz, Ingest: pipe,
				Tokens: tokens, ExternalURL: c.Server.ExternalURL, MaxManifestSize: c.Server.MaxManifestSize, Log: a.log,
				UploadSessionsPerRepo: c.Server.UploadSessions.PerRepo, UploadSessionsPerPrincipal: c.Server.UploadSessions.PerPrincipal})
			authn := auth.NewAuthenticator(auth.Options{Index: st, Log: a.log})
			proxies, err := c.Server.TrustedPrefixes()
			if err != nil {
				return err
			}

			srv, err := server.New(server.Options{
				Listen:          c.Server.Listen,
				ExternalURL:     c.Server.ExternalURL,
				TLSCertFile:     c.Server.TLS.CertFile,
				TLSKeyFile:      c.Server.TLS.KeyFile,
				ShutdownTimeout: c.Server.ShutdownTimeout,
				Log:             a.log,
				Plugins:         servedPlugins(c.Server.ExternalURL, accounts{authn, &auth.Tokens{Index: st, Log: a.log}}),
				Deps:            deps,
				Ready:           ready(a.log, c.Database.Driver, st, health, rep),
				API:             api.New(api.Options{Repos: repos, Auth: authz, Reindex: runs, Deleter: pipe, Bootstrap: boot, Log: a.log}),
				Facade:          v2,
				Authenticator:   authn,
				MetricsListen:   c.Server.MetricsListen,
				Pprof:           c.Server.Pprof,
				StallTimeout:    c.Server.StallTimeout,
				MaxUploads:      c.Server.MaxConcurrentUploads,
				TrustedProxies:  proxies,
			})
			if err != nil {
				return err
			}
			return srv.Run(ctx)
		},
	}
}

// probeRegistries asks every registry for its capabilities, all at once and each for at most
// registryProbeTimeout; the providers log what they find. A registry that does not answer is
// logged at error and serve starts anyway: one backend down must not keep a replica from
// starting. Readiness reports the registries with issue #23.
func probeRegistries(ctx context.Context, providers provider.Map, log *slog.Logger) {
	var wg sync.WaitGroup
	for name, p := range providers {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, registryProbeTimeout)
			defer cancel()
			if _, err := p.Capabilities(ctx); err != nil {
				log.ErrorContext(ctx, "registry capabilities unknown; serving anyway", "registry", name, "error", err.Error())
			}
		})
	}
	wg.Wait()
}

// ready is the readiness check of serve (docs/spec/http-routing.md §5): the index database
// answers, no other replica serves on it when it is SQLite, this replica's clock is within
// clockFail of the database's, and every registry answered GET /v2/ within the last minute. A failure is logged with its cause and answered without it: /-/readyz
// is public, and a driver error can name hosts and users; the registry probe and the heartbeat of
// the replica log their own failures.
func ready(log *slog.Logger, driver string, st store, health *registryHealth, rep *replica) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := st.Ping(ctx); err != nil {
			log.WarnContext(ctx, "index database ping failed", "database.driver", driver, "error", err.Error())
			return errors.New("index database is unreachable")
		}
		if err := rep.Err(); err != nil {
			return err
		}
		if stale := health.Stale(); len(stale) > 0 {
			log.DebugContext(ctx, "not ready: registries without an answer for a minute", "registries", stale)
			return errors.New("a registry has not answered for a minute")
		}
		return nil
	}
}

// backgroundReindex starts the runs of POST …/reindex (api.Reindexer) on the server's context,
// so that the shutdown cancels them, and lets serve wait for them before it closes the index.
type backgroundReindex struct {
	w   *reindex.Worker
	ctx context.Context
	log *slog.Logger
	wg  sync.WaitGroup
}

// Start takes the lock of the run's scope and runs it in the background.
func (b *backgroundReindex) Start(ctx context.Context, req reindex.Request) (*index.ReindexRun, error) {
	run, err := b.w.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	b.wg.Go(func() {
		if _, err := b.w.Run(b.ctx, run, req); err != nil {
			b.log.ErrorContext(b.ctx, "reindex: a run started through the API failed", "run", run.ID, "error", err.Error())
		}
	})
	return run, nil
}

// sessionExpiryInterval is how often serve marks the sessions whose time is up as expired.
const sessionExpiryInterval = time.Minute

// expireSessions marks the sessions whose time is up as expired every interval until ctx ends
// (ingest.Pipeline.ExpireSessions). The sessions do not depend on it, it keeps their rows true;
// a failure is logged and the next tick tries again.
func expireSessions(ctx context.Context, pipe *ingest.Pipeline, log *slog.Logger, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := pipe.ExpireSessions(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			log.WarnContext(ctx, "sessions not expired", "error", err.Error())
		case n > 0:
			log.InfoContext(ctx, "sessions expired: their files are dropped", "count", n)
		}
	}
}
