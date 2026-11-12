package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/config"
)

// app is the state the commands share: the persistent flags, and the configuration and logger
// that the root command's PersistentPreRunE sets up once.
type app struct {
	lookupEnv  func(string) (string, bool)
	configPath string
	logLevel   string
	logFormat  string

	cfg *config.Config // loaded only for the commands marked with needsConfig
	log *slog.Logger
	now func() time.Time // the clock checkClock compares with the database's; nil means time.Now
}

// clock returns the time of a.now.
func (a *app) clock() time.Time {
	if a.now == nil {
		return time.Now()
	}
	return a.now()
}

// needsConfig marks a command whose PersistentPreRunE loads and validates the configuration.
var needsConfig = map[string]string{"lajstrom/config": "required"}

// newRootCmd builds the command tree. lookupEnv is os.LookupEnv outside tests.
func newRootCmd(lookupEnv func(string) (string, bool)) *cobra.Command {
	a := &app{lookupEnv: lookupEnv}
	root := &cobra.Command{
		Use:   "lajstrom",
		Short: "Artifact registry on top of any OCI registry",
		Long: "Lajstrom serves raw, pypi, apt, maven and npm repositories and stores every artifact in\n" +
			"an OCI registry. See https://lajstrom.dev and docs/configuration.md.",
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: a.setup,
	}
	configDefault := "lajstrom.yaml"
	if v, ok := lookupEnv("LAJSTROM_CONFIG"); ok && v != "" {
		configDefault = v
	}
	f := root.PersistentFlags()
	f.StringVar(&a.configPath, "config", configDefault, "configuration file (env LAJSTROM_CONFIG)")
	f.StringVar(&a.logLevel, "log-level", "", "debug, info, warn or error (default: log.level of the configuration, else info)")
	f.StringVar(&a.logFormat, "log-format", "", "text or json (default: log.format of the configuration, else text)")

	root.AddCommand(
		newServeCmd(a),
		newMigrateCmd(a),
		newReindexCmd(a),
		newVersionCmd(),
		newAdminCmd(a),
		newHealthcheckCmd(a),
		newRegistryCmd(a),
	)
	for _, c := range root.Commands() {
		refuseUnknown(c)
	}
	return root
}

// refuseUnknown gives c and every group below it that has no Run of its own unknownSubcommand:
// cobra prints the help of such a group and returns nil, so `admin token revok 2` exited 0 and
// revoked nothing (#716). The root refuses an unknown command by itself.
func refuseUnknown(c *cobra.Command) {
	if c.HasSubCommands() && !c.Runnable() {
		c.RunE = unknownSubcommand
	}
	for _, sub := range c.Commands() {
		refuseUnknown(sub)
	}
}

// unknownSubcommand is the RunE of a group: without an argument it prints the help, as before;
// an argument names a subcommand the group does not have, an error (exit 1) with cobra's
// suggestions.
func unknownSubcommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
		msg += "\n\nDid you mean this?\n\t" + strings.Join(s, "\n\t")
	}
	return errors.New(msg)
}

// setup loads the configuration for the commands that need it, then builds the logger from
// the flags, falling back to the configuration and the defaults.
func (a *app) setup(cmd *cobra.Command, _ []string) error {
	level, format := config.DefaultLogLevel, config.DefaultLogFormat
	if cmd.Annotations["lajstrom/config"] == "required" {
		cfg, err := config.Load(a.configPath, a.lookupEnv)
		if errors.Is(err, fs.ErrNotExist) {
			return configError{fmt.Errorf("%w (choose the file with --config or LAJSTROM_CONFIG)", err)}
		}
		if err != nil {
			return configError{err}
		}
		a.cfg = cfg
		level, format = cfg.Log.Level, cfg.Log.Format
	}
	if a.logLevel != "" {
		level = a.logLevel
	}
	if a.logFormat != "" {
		format = a.logFormat
	}
	log, err := newLogger(cmd.ErrOrStderr(), level, format)
	if err != nil {
		return err
	}
	a.log = log
	return nil
}

// configError is a configuration that cannot be read or is invalid: the process exits with
// status 2 for it, 1 for any other error.
type configError struct{ error }

func (e configError) Unwrap() error { return e.error }

// exitStatus is the exit status of the process for the error of a command: an exitCode's own.
func exitStatus(err error) int {
	var code exitCode
	switch {
	case errors.As(err, &code):
		return int(code)
	case errors.As(err, new(configError)):
		return 2
	}
	return 1
}
