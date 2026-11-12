package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// prefetchBatch is how many names one request of admin repo prefetch sends by default, and
// prefetchTimeout how long one request may take by default: the server answers once every
// version of its names has ended, and with --all-versions a name may have hundreds.
const (
	prefetchBatch   = 20
	prefetchTimeout = 30 * time.Minute
)

// prefetchReport is what admin repo prefetch did: the JSON of --report, which --failed reads
// back. Results are the server's, and a failed one per name of a batch whose request failed.
type prefetchReport struct {
	Repo        string                 `json:"repo"`
	AllVersions bool                   `json:"allVersions"`
	Names       int                    `json:"names"`
	Counts      map[string]int         `json:"counts"`
	Results     []proxy.PrefetchResult `json:"results"`
}

// usageError is an error of the arguments of admin repo prefetch: exit status 2, as for a
// configuration error.
func usageError(format string, args ...any) error { return configError{fmt.Errorf(format, args...)} }

// newRepoPrefetchCmd returns admin repo prefetch: a client of the management API that fetches
// the versions of a list of names into a proxy repo (docs/spec/proxy-and-group.md §5.1,
// POST …/prefetch) in batches, prints a line per version as each batch is answered and a count
// at the end, and writes the results as JSON with --report. --failed reads such a report
// instead of a list and sends again the names of its failed results. A batch whose request
// fails makes its names failed and stops the run, as does SIGINT, SIGTERM or SIGHUP; the names
// left are failed, not attempted. The server and the token are those of admin retention: --url
// or LAJSTROM_URL, --token-file, LAJSTROM_TOKEN_FILE or LAJSTROM_TOKEN; the configuration is not
// read. Exit status 0: every version fetched, held or shared; 1: a failed one, or an error;
// 2: a usage error.
func newRepoPrefetchCmd(a *app) *cobra.Command {
	var (
		from, report, base, tokenFile string
		all, failed                   bool
		batch                         int
		timeout                       time.Duration
	)
	cmd := &cobra.Command{
		Use:   "prefetch <format>/<name> --from-file <names|-> [--all-versions] [--failed] [--batch 20] [--report out.json]",
		Short: "Fetch the versions of a list of names into a proxy repo, through the server's API",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageError("prefetch takes one argument, <format>/<name>; it has %d", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			f, name, err := splitRepo(args[0])
			switch {
			case err != nil:
				return configError{err}
			case from == "":
				return usageError("--from-file: name the file of the names, or - for standard input")
			case batch < 1 || batch > proxy.MaxPrefetchNames:
				return usageError("--batch: %d is not 1 to %d", batch, proxy.MaxPrefetchNames)
			case timeout <= 0:
				return usageError("--timeout: %s is not positive", timeout)
			}
			names, err := prefetchNames(cmd.InOrStdin(), from, failed)
			if err != nil {
				return usageError("--from-file: %w", err)
			}
			c, err := newRetentionClient(a.lookupEnv, base, tokenFile)
			if err != nil {
				return usageError("%w", err)
			}
			c.timeout = timeout
			ctx, stop := retentionContext(cmd.Context())
			defer stop()
			p := &prefetch{client: c, format: f, name: name, all: all, out: cmd.OutOrStdout()}
			rep := p.run(ctx, names, batch)
			if report != "" {
				if err := writePrefetchReport(report, rep); err != nil {
					return err
				}
			}
			if rep.Counts[proxy.PrefetchFailed] > 0 {
				return exitCode(1)
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&from, "from-file", "", "the names, one a line (# starts a comment); - for standard input; with --failed a --report of an earlier run")
	fl.BoolVar(&all, "all-versions", false, "every version the upstream lists; without it, those its dist-tags name")
	fl.BoolVar(&failed, "failed", false, "--from-file is a report: send again the names of its failed results")
	fl.IntVar(&batch, "batch", prefetchBatch, fmt.Sprintf("names per request, 1 to %d", proxy.MaxPrefetchNames))
	fl.DurationVar(&timeout, "timeout", prefetchTimeout, "how long one request may take")
	fl.StringVar(&report, "report", "", "write the results to this file as JSON")
	fl.StringVar(&base, "url", "", "the server's base URL (env "+envRetentionURL+")")
	fl.StringVar(&tokenFile, "token-file", "", "a file with a personal access token (env "+envRetentionTokenFile+", or the token in "+envRetentionToken+")")
	return cmd
}

// prefetchNames reads the names of from, a file or - for stdin: one a line, without the blank
// lines and what follows #, each once in the order of its first line; or, with failed, the
// names of the failed results of a report. No name is an error.
func prefetchNames(stdin io.Reader, from string, failed bool) ([]string, error) {
	r := stdin
	if from != "-" {
		f, err := os.Open(from) //nolint:gosec // the operator names the file
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	var names []string
	if failed {
		var rep prefetchReport
		if err := json.NewDecoder(r).Decode(&rep); err != nil {
			return nil, fmt.Errorf("not a report of admin repo prefetch: %w", err)
		}
		for _, res := range rep.Results {
			if res.Status == proxy.PrefetchFailed {
				names = append(names, res.Name)
			}
		}
	} else {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line, _, _ := strings.Cut(sc.Text(), "#")
			if line = strings.TrimSpace(line); line != "" {
				names = append(names, line)
			}
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	out := names[:0]
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no name in it")
	}
	return out, nil
}

// prefetch is one run.
type prefetch struct {
	client       *retentionClient
	format, name string
	all          bool
	out          io.Writer
}

// run sends names in batches and prints each batch's results as it is answered; a request that
// fails, or a context that ends, fails the names left. It returns the report.
func (p *prefetch) run(ctx context.Context, names []string, batch int) prefetchReport {
	rep := prefetchReport{Repo: p.format + "/" + p.name, AllVersions: p.all, Names: len(names), Counts: map[string]int{},
		Results: []proxy.PrefetchResult{}}
	target := "/api/v1/repos/" + url.PathEscape(p.format) + "/" + url.PathEscape(p.name) + "/prefetch"
	stopped := "" // why the names left are not attempted
	for start := 0; start < len(names); start += batch {
		part := names[start:min(start+batch, len(names))]
		var results []proxy.PrefetchResult
		var why string
		switch {
		case stopped != "":
			why = stopped
		case ctx.Err() != nil:
			why, stopped = "not attempted: the run was stopped", "not attempted: the run was stopped"
		default:
			var answer api.PrefetchAnswer
			err := p.client.do(ctx, p.client.timeout, "POST", target, api.PrefetchRequest{Names: part, AllVersions: p.all}, &answer)
			if err != nil {
				why, stopped = err.Error(), "not attempted: an earlier request failed"
			}
			results = answer.Results
		}
		if why != "" {
			results = make([]proxy.PrefetchResult, len(part))
			for i, n := range part {
				results[i] = proxy.PrefetchResult{Name: n, Status: proxy.PrefetchFailed, Message: why}
			}
		}
		for _, r := range results {
			rep.Counts[r.Status]++
			_, _ = fmt.Fprintln(p.out, resultLine(r))
		}
		rep.Results = append(rep.Results, results...)
	}
	_, _ = fmt.Fprintf(p.out, "%s: %d names: %d fetched, %d held, %d shared, %d failed\n", rep.Repo, rep.Names, rep.Counts["fetched"],
		rep.Counts["held"], rep.Counts["shared"], rep.Counts[proxy.PrefetchFailed])
	return rep
}

// resultLine is the line of a result: its status, its name and version, and for a failure its
// status code and message.
func resultLine(r proxy.PrefetchResult) string {
	line := strings.TrimSpace(fmt.Sprintf("%-7s  %s %s", r.Status, r.Name, r.Version))
	switch {
	case r.HTTPStatus != 0:
		line += fmt.Sprintf(": %d %s", r.HTTPStatus, r.Message)
	case r.Message != "":
		line += ": " + r.Message
	}
	return line
}

// writePrefetchReport writes rep to path as indented JSON.
func writePrefetchReport(path string, rep prefetchReport) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("--report: %w", err)
	}
	return nil
}
