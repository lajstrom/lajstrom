package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// newReindexCmd returns reindex, which rebuilds the derived index tables from the manifests in
// the registries (#29; docs/operations.md, Reindex). It runs in this process, under the lock
// that keeps it apart from a run of the same repos on a replica, as the anonymous principal
// with the source cli, like admin. It assumes that lajstrom migrate ran. The exit status is not
// zero when the run failed or another run holds the lock; the manifests that could not be read
// or checked are in the report.
func newReindexCmd(a *app) *cobra.Command {
	var (
		repoFlag    string
		prune       bool
		verify      bool
		report      bool
		asJSON      bool
		concurrency int
	)
	cmd := &cobra.Command{
		Use:   "reindex",
		Short: "Rebuild the index from the manifests in the registries",
		Long: "Rebuild the index from the manifests in the registries: every repo, or one with --repo.\n" +
			"A manifest whose digest the index has is not read again unless --verify. With --prune (the\n" +
			"default for every repo) the rows of manifests that are gone from the registry are removed.\n" +
			"--report lists foreign manifests, orphan referrers, drift and errors; --json prints the report.",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			c := a.cfg
			st, err := openStore(ctx, c.Database)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, st.Close()) }()
			req := reindex.Request{Prune: prune, Verify: verify, Source: "cli", Principal: auth.Anonymous.String()}
			if !cmd.Flags().Changed("prune") {
				req.Prune = repoFlag == ""
			}
			if repoFlag != "" {
				if req.Repo, err = lookupRepo(ctx, st, repoFlag); err != nil {
					return err
				}
			}
			providers, err := provider.NewSet(c.Registries, a.log)
			if err != nil {
				return err
			}
			pipe := ingest.New(ingest.Options{Providers: providers, Index: st, Log: a.log, ClaimTTL: c.Ingest.ClaimTTL})
			w := reindex.New(reindex.Options{Index: st, Providers: providers, Inspector: pipe, Log: a.log,
				Concurrency: concurrency, ClaimTTL: c.Ingest.ClaimTTL})
			run, err := w.Start(ctx, req)
			var conflict *index.ReindexConflictError
			if errors.As(err, &conflict) {
				return fmt.Errorf("another reindex holds the lock: it runs on %s since %s (run %d)", conflict.Running.Replica,
					conflict.Running.StartedAt.Format("2006-01-02T15:04:05Z07:00"), conflict.Running.ID)
			}
			if err != nil {
				return err
			}
			a.log.InfoContext(ctx, "reindex: started", "run", run.ID, "repo", repoFlag, "prune", req.Prune, "verify", req.Verify)
			rep, err := w.Run(ctx, run, req)
			if rep != nil {
				var werr error
				if asJSON {
					werr = writeJSONLine(cmd.OutOrStdout(), rep)
				} else {
					werr = writeReport(cmd.OutOrStdout(), rep, report)
				}
				err = errors.Join(err, werr)
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&repoFlag, "repo", "", "the repo to reindex, <format>/<name>; every repo when empty")
	f.BoolVar(&prune, "prune", false, "remove the rows of manifests that are gone (default true without --repo)")
	f.BoolVar(&verify, "verify", false, "read and check every manifest, also those whose digest the index has")
	f.BoolVar(&report, "report", false, "list foreign manifests, orphan referrers, drift and errors")
	f.BoolVar(&asJSON, "json", false, "print the report as JSON")
	f.IntVar(&concurrency, "concurrency", 0, "manifests read at once per registry (default 8)")
	return cmd
}

// lookupRepo returns the repo <format>/<name>.
func lookupRepo(ctx context.Context, st index.Reader, s string) (*index.Repo, error) {
	f, name, ok := strings.Cut(s, "/")
	if !ok || f == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("--repo %q is not <format>/<name>", s)
	}
	repo, err := st.GetRepo(ctx, f, name)
	if errors.Is(err, index.ErrNotFound) {
		return nil, fmt.Errorf("repo %s does not exist", s)
	}
	return repo, err
}

// writeReport prints one line of counts per repo and, with lists, the findings below it.
func writeReport(out io.Writer, rep *reindex.Report, lists bool) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	// A bytes.Buffer does not fail.
	_, _ = io.WriteString(tw, "REPO\tMANIFESTS\tADDED\tUPDATED\tUNCHANGED\tREMOVED\tDEGRADED\tREFERRERS\tFOREIGN\tORPHANS\tDRIFT\tERRORS\n")
	for _, r := range rep.Repos {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", r.Repo, r.Manifests, r.Added, r.Updated,
			r.Unchanged, r.Removed, r.Degraded, r.Referrers, len(r.Foreign), len(r.OrphanReferrers), len(r.Drift), len(r.Errors))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if lists {
		for _, r := range rep.Repos {
			for _, f := range r.Foreign {
				fmt.Fprintf(&buf, "%s: foreign %s:%s %s %s\n", r.Repo, f.Repository, f.Tag, f.Digest, f.MediaType)
			}
			for _, f := range r.OrphanReferrers {
				fmt.Fprintf(&buf, "%s: orphan referrer %s:%s %s of %s\n", r.Repo, f.Repository, f.Tag, f.Digest, f.Subject)
			}
			for _, d := range r.Drift {
				fmt.Fprintf(&buf, "%s: drift %s:%s index %s, registry %s\n", r.Repo, d.Package, d.Tag, d.Index, d.Registry)
			}
			for _, f := range r.Errors {
				fmt.Fprintf(&buf, "%s: error %s:%s %s\n", r.Repo, f.Repository, f.Tag, f.Error)
			}
			if r.Truncated {
				fmt.Fprintf(&buf, "%s: the lists stop at 1000 entries each; --json has the same\n", r.Repo)
			}
		}
	}
	_, err := out.Write(buf.Bytes())
	return err
}
