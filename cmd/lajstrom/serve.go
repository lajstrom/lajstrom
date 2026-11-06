package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
	_ "github.com/lajstrom/lajstrom/internal/provider/generic" // registers registries[].type generic
	"github.com/lajstrom/lajstrom/internal/server"
	"github.com/lajstrom/lajstrom/internal/version"
)

// registryProbeTimeout bounds the capability probe of each registry at startup.
const registryProbeTimeout = 10 * time.Second

// newServeCmd returns serve: load and validate the configuration (in the root's
// PersistentPreRunE), build a provider for every registry and probe its capabilities, build
// the server and run it until SIGINT or SIGTERM.
//
// serve does not open the index database yet: issue #10 wires the index (and readiness).
// Until then only the system endpoints answer.
func newServeCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "serve",
		Short:       "Run the HTTP server",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			c, v := a.cfg, version.Get()
			a.log.Info("starting lajstrom; the index database is not wired yet (issue #10)",
				"version", v.Version,
				"commit", v.Commit,
				"config", a.configPath,
				"registries", len(c.Registries),
				"repos", len(c.Repos),
			)
			providers, err := provider.NewSet(c.Registries, a.log)
			if err != nil {
				return err
			}
			probeRegistries(ctx, providers, a.log)
			srv, err := server.New(server.Options{
				Listen:          c.Server.Listen,
				ExternalURL:     c.Server.ExternalURL,
				ShutdownTimeout: c.Server.ShutdownTimeout,
				Log:             a.log,
				Plugins:         format.All(),
				Deps:            format.Deps{Providers: providers},
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
