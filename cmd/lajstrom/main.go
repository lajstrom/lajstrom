// Command lajstrom is the Lajstrom server and its command-line tools: serve, migrate, reindex,
// version, admin and healthcheck. Run lajstrom --help for the commands; docs/configuration.md
// describes the configuration file.
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd(os.LookupEnv).ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "lajstrom:", err)
		os.Exit(exitStatus(err))
	}
}
