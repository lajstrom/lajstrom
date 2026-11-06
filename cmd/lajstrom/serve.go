package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/server"
	"github.com/lajstrom/lajstrom/internal/version"
)

// newServeCmd returns serve: load and validate the configuration (in the root's
// PersistentPreRunE), build the server and run it until SIGINT or SIGTERM.
//
// serve does not open the index database and does not connect to the registries yet: issue
// #10 wires the index (and readiness), issue #9 the registries. Until then only the system
// endpoints answer.
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
			a.log.Info("starting lajstrom; index database and registries are not wired yet (issues #10, #9)",
				"version", v.Version,
				"commit", v.Commit,
				"config", a.configPath,
				"registries", len(c.Registries),
				"repos", len(c.Repos),
			)
			srv, err := server.New(server.Options{
				Listen:          c.Server.Listen,
				ExternalURL:     c.Server.ExternalURL,
				ShutdownTimeout: c.Server.ShutdownTimeout,
				Log:             a.log,
				Plugins:         format.All(),
			})
			if err != nil {
				return err
			}
			return srv.Run(ctx)
		},
	}
}
