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
	"github.com/lajstrom/lajstrom/internal/facade"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	_ "github.com/lajstrom/lajstrom/internal/provider/generic" // registers registries[].type generic
	"github.com/lajstrom/lajstrom/internal/server"
	"github.com/lajstrom/lajstrom/internal/version"
)

// registryProbeTimeout bounds the capability probe of each registry at startup.
const registryProbeTimeout = 10 * time.Second

// newServeCmd returns serve: load and validate the configuration (in the root's
// PersistentPreRunE), open the index database and apply its pending migrations, create the
// repos of the configuration that the control plane does not have, load the master key, build
// a provider for every registry and probe its capabilities, build the server and run it until
// SIGINT or SIGTERM, then close the database.
//
// Migrating on start takes the same lock as lajstrom migrate, so replicas that start together
// apply each migration once. The index is Deps.Index of the format plugins and /-/readyz pings
// it; the registries are Deps.Providers; the ingest pipeline over both, with ingest.claimTTL,
// is Deps.Ingest; the signing keys over the index and the master key (newSigner) are
// Deps.Signer. The repo service over the index (api.Repos) runs the config bootstrap
// before the server listens, and serves /api/v1. The OCI facade over the index and the
// registries serves /v2/, and hands the manifests of managed repos to the same pipeline;
// server.maxManifestSize bounds them.
func newServeCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "serve",
		Short:       "Run the HTTP server",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			c, v := a.cfg, version.Get()
			a.log.Info("starting lajstrom",
				"version", v.Version,
				"commit", v.Commit,
				"config", a.configPath,
				"database.driver", c.Database.Driver,
				"registries", len(c.Registries),
				"repos", len(c.Repos),
			)
			st, err := openStore(ctx, c.Database)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, st.Close()) }()
			applied, err := st.ApplyMigrations(ctx)
			if err != nil {
				return err
			}
			a.log.Info("index migrations applied under the migration lock",
				"database.driver", c.Database.Driver, "applied", applied)
			repos := newRepos(c, st, a.log)
			created, err := repos.Bootstrap(ctx, c.Repos)
			if err != nil {
				return err
			}
			a.log.Info("repos of the configuration bootstrapped", "configured", len(c.Repos), "created", created)
			signer, err := newSigner(ctx, c, a.lookupEnv, st, a.log)
			if err != nil {
				return err
			}

			providers, err := provider.NewSet(c.Registries, a.log)
			if err != nil {
				return err
			}
			probeRegistries(ctx, providers, a.log)
			pipe := ingest.New(ingest.Options{Providers: providers, Index: st, Log: a.log, ClaimTTL: c.Ingest.ClaimTTL})
			v2 := facade.New(facade.Options{Index: st, Providers: providers, Ingest: pipe,
				MaxManifestSize: c.Server.MaxManifestSize, Log: a.log})

			srv, err := server.New(server.Options{
				Listen:          c.Server.Listen,
				ExternalURL:     c.Server.ExternalURL,
				ShutdownTimeout: c.Server.ShutdownTimeout,
				Log:             a.log,
				Plugins:         format.All(),
				Deps:            format.Deps{Index: st, Providers: providers, Ingest: pipe, Signer: signer},
				Ready:           ready(a.log, c.Database.Driver, st),
				API:             api.New(api.Options{Repos: repos, Log: a.log}),
				Facade:          v2,
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

// ready is the readiness check of serve: the index database answers. A failure is logged with
// its cause and answered without it: /-/readyz is public, and a driver error can name hosts
// and users.
func ready(log *slog.Logger, driver string, st store) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := st.Ping(ctx); err != nil {
			log.WarnContext(ctx, "index database ping failed", "database.driver", driver, "error", err.Error())
			return errors.New("index database is unreachable")
		}
		return nil
	}
}
