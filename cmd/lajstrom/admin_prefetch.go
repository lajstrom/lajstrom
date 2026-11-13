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
	"slices"
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
// Platforms are those each image of an oci repo was sent with, which --failed sends again.
type prefetchReport struct {
	Repo        string                 `json:"repo"`
	AllVersions bool                   `json:"allVersions"`
	Names       int                    `json:"names"`
	Counts      map[string]int         `json:"counts"`
	Results     []proxy.PrefetchResult `json:"results"`
	Platforms   map[string][]string    `json:"platforms,omitempty"`
	Skipped     []string               `json:"skipped,omitempty"`
}

// prefetchFormatOCI is the format whose proxy repos take images instead of names.
const prefetchFormatOCI = "oci"

// usageError is an error of the arguments of admin repo prefetch: exit status 2, as for a
// configuration error.
func usageError(format string, args ...any) error { return configError{fmt.Errorf(format, args...)} }

// newRepoPrefetchCmd returns admin repo prefetch: a client of the management API that fetches
// the versions of a list of names into a proxy repo (docs/spec/proxy-and-group.md §5.1,
// POST …/prefetch), or the images of a list of references into an oci proxy repo (§11.10), in
// batches, prints a line per result as each batch is answered and a count at the end, and
// writes the results as JSON with --report. --failed reads such a report instead of a list and
// sends again the names of its failed results. A batch whose request fails makes its names
// failed and stops the run, as does SIGINT, SIGTERM or SIGHUP; the names left are failed, not
// attempted. Of an oci repo, --platform names the platforms of every image whose line names
// none, and --host keeps the references of one registry and strips it. The server and the token
// are those of admin retention: --url or LAJSTROM_URL, --token-file, LAJSTROM_TOKEN_FILE or
// LAJSTROM_TOKEN; the configuration is not read. Exit status 0: every result fetched, held or
// shared; 1: a failed one, or an error; 2: a usage error.
func newRepoPrefetchCmd(a *app) *cobra.Command {
	var (
		o                       prefetchOptions
		report, base, tokenFile string
		timeout                 time.Duration
	)
	cmd := &cobra.Command{
		Use: "prefetch <format>/<name> --from-file <names|-> [--all-versions] [--failed] [--batch 20] [--report out.json]\n" +
			"  prefetch oci/<name> --from-file <images|-> --platform linux/amd64,linux/arm64 [--host docker.io] [--failed] [--batch 20] [--report out.json]",
		Short: "Fetch the versions of a list of names, or a list of images, into a proxy repo, through the server's API",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageError("prefetch takes one argument, <format>/<name>; it has %d", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			f, name, err := splitRepo(args[0])
			if err != nil {
				return configError{err}
			}
			if timeout <= 0 {
				return usageError("--timeout: %s is not positive", timeout)
			}
			if err := o.check(f, args[0]); err != nil {
				return err
			}
			groups, skipped, err := o.read(cmd.InOrStdin(), f)
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
			p := &prefetch{client: c, format: f, name: name, all: o.all, out: cmd.OutOrStdout()}
			rep := p.run(ctx, groups, o.batch)
			rep.Skipped = skipped
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
	fl.StringVar(&o.from, "from-file", "", "the names, one a line (# starts a comment); - for standard input; with --failed a --report of an earlier run")
	fl.BoolVar(&o.all, "all-versions", false, "every version the upstream lists; without it, those its dist-tags name")
	fl.StringSliceVar(&o.platforms, "platform", nil, "of an oci repo, the platforms of an index (os/arch[/variant]), for each image whose line names none")
	fl.StringVar(&o.host, "host", "", "of an oci repo, keep the references of this registry and strip it (docker.io and index.docker.io are Docker Hub's)")
	fl.BoolVar(&o.failed, "failed", false, "--from-file is a report: send again the names of its failed results")
	fl.IntVar(&o.batch, "batch", prefetchBatch, fmt.Sprintf("names per request, 1 to %d", proxy.MaxPrefetchNames))
	fl.DurationVar(&timeout, "timeout", prefetchTimeout, "how long one request may take")
	fl.StringVar(&report, "report", "", "write the results to this file as JSON")
	fl.StringVar(&base, "url", "", "the server's base URL (env "+envRetentionURL+")")
	fl.StringVar(&tokenFile, "token-file", "", "a file with a personal access token (env "+envRetentionTokenFile+", or the token in "+envRetentionToken+")")
	return cmd
}

// prefetchOptions are what admin repo prefetch reads and how it sends it.
type prefetchOptions struct {
	from, host  string
	all, failed bool
	batch       int
	platforms   []string
}

// check refuses the options that do not go together, or with the format f of repo.
func (o *prefetchOptions) check(f, repo string) error {
	switch {
	case o.from == "":
		return usageError("--from-file: name the file of the names, or - for standard input")
	case o.batch < 1 || o.batch > proxy.MaxPrefetchNames:
		return usageError("--batch: %d is not 1 to %d", o.batch, proxy.MaxPrefetchNames)
	case f == prefetchFormatOCI && o.all:
		return usageError("--all-versions: an oci repo takes images and their platforms")
	case f != prefetchFormatOCI && (len(o.platforms) > 0 || o.host != ""):
		return usageError("--platform and --host are for an oci repo; %s takes names", repo)
	}
	if err := checkPlatforms(o.platforms); err != nil {
		return usageError("--platform: %w", err)
	}
	return nil
}

// read reads the names of a repo of format f, or its images by their platforms, and the
// references skipped for --host.
func (o *prefetchOptions) read(stdin io.Reader, f string) ([]prefetchGroup, []string, error) {
	if f == prefetchFormatOCI {
		return prefetchImages(stdin, o.from, o.failed, o.platforms, o.host)
	}
	names, err := prefetchNames(stdin, o.from, o.failed)
	return []prefetchGroup{{names: names}}, nil, err
}

// openFrom opens from, a file or - for stdin, and returns it with its close.
func openFrom(stdin io.Reader, from string) (io.Reader, func(), error) {
	if from == "-" {
		return stdin, func() {}, nil
	}
	f, err := os.Open(from) //nolint:gosec // the operator names the file
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// prefetchNames reads the names of from, a file or - for stdin: one a line, without the blank
// lines and what follows #, each once in the order of its first line; or, with failed, the
// names of the failed results of a report. No name is an error.
func prefetchNames(stdin io.Reader, from string, failed bool) ([]string, error) {
	r, closeFrom, err := openFrom(stdin, from)
	if err != nil {
		return nil, err
	}
	defer closeFrom()
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

// prefetchGroup is names that go with the same platforms: those of an oci repo's images, none
// of a format's names.
type prefetchGroup struct {
	platforms []string
	names     []string
}

// prefetch is one run.
type prefetch struct {
	client       *retentionClient
	format, name string
	all          bool
	out          io.Writer
}

// run sends the names of each group in batches and prints each batch's results as it is
// answered; a request that fails, or a context that ends, fails the names left. It returns the
// report.
func (p *prefetch) run(ctx context.Context, groups []prefetchGroup, batch int) prefetchReport {
	rep := prefetchReport{Repo: p.format + "/" + p.name, AllVersions: p.all, Counts: map[string]int{}, Results: []proxy.PrefetchResult{}}
	target := "/api/v1/repos/" + url.PathEscape(p.format) + "/" + url.PathEscape(p.name) + "/prefetch"
	stopped := "" // why the names left are not attempted
	for _, g := range groups {
		rep.Names += len(g.names)
		for start := 0; start < len(g.names); start += batch {
			part := g.names[start:min(start+batch, len(g.names))]
			req := api.PrefetchRequest{Names: part, AllVersions: p.all}
			if p.format == prefetchFormatOCI {
				req = api.PrefetchRequest{Images: part, Platforms: g.platforms}
				for _, n := range part {
					if len(g.platforms) > 0 {
						if rep.Platforms == nil {
							rep.Platforms = map[string][]string{}
						}
						rep.Platforms[n] = g.platforms
					}
				}
			}
			var results []proxy.PrefetchResult
			var why string
			switch {
			case stopped != "":
				why = stopped
			case ctx.Err() != nil:
				why, stopped = "not attempted: the run was stopped", "not attempted: the run was stopped"
			default:
				var answer api.PrefetchAnswer
				if err := p.client.do(ctx, p.client.timeout, "POST", target, req, &answer); err != nil {
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
	}
	what := "names"
	if p.format == prefetchFormatOCI {
		what = "images"
	}
	_, _ = fmt.Fprintf(p.out, "%s: %d %s: %d fetched, %d held, %d shared, %d failed\n", rep.Repo, rep.Names, what, rep.Counts["fetched"],
		rep.Counts["held"], rep.Counts["shared"], rep.Counts[proxy.PrefetchFailed])
	return rep
}

// resultLine is the line of a result: its status, its name and version, or of an oci repo its
// name, kind, platform, digest and the bytes fetched, and for a failure its status code, its
// message and when to ask again.
func resultLine(r proxy.PrefetchResult) string {
	parts := []string{fmt.Sprintf("%-7s ", r.Status), r.Name}
	for _, s := range []string{r.Version, r.Kind, r.Platform, r.Digest.String()} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	line := strings.Join(parts, " ")
	if r.Bytes > 0 {
		line += fmt.Sprintf(" (%d bytes)", r.Bytes)
	}
	switch {
	case r.HTTPStatus != 0:
		line += fmt.Sprintf(": %d %s", r.HTTPStatus, r.Message)
	case r.Message != "":
		line += ": " + r.Message
	}
	if r.RetryAfter > 0 {
		line += fmt.Sprintf(" (retry after %ds)", r.RetryAfter)
	}
	return line
}

// prefetchImage is an image of an oci repo and the platforms it is sent with.
type prefetchImage struct {
	ref       string
	platforms []string
}

// prefetchImages reads the images of an oci repo from from, a file or - for stdin: one a line,
// <reference> [<platform>,…], without the blank lines and what follows #, each once in the order
// of its first line, with the line's platforms or else platforms; with host, only the references
// of that registry, which is stripped, and those of the others skipped. With failed, from is a
// report: the images of its failed results, each with the platforms the report gives it or else
// platforms, and host is not applied, since the report's references are below the repo. It
// returns them grouped by their platforms, in the order of each group's first image.
func prefetchImages(stdin io.Reader, from string, failed bool, platforms []string, host string) (groups []prefetchGroup, skipped []string, err error) {
	r, closeFrom, err := openFrom(stdin, from)
	if err != nil {
		return nil, nil, err
	}
	defer closeFrom()
	var images []prefetchImage
	if failed {
		images, err = failedImages(r, platforms)
	} else {
		images, skipped, err = imageLines(r, platforms, host)
	}
	if err != nil {
		return nil, nil, err
	}
	if groups = groupImages(images); len(groups) == 0 {
		if len(skipped) > 0 {
			return nil, nil, fmt.Errorf("no reference of %s in it; %d of other registries", host, len(skipped))
		}
		return nil, nil, errors.New("no image in it")
	}
	return groups, skipped, nil
}

// failedImages reads a report and returns the images of its failed results, each with the
// platforms the report gives it or else platforms.
func failedImages(r io.Reader, platforms []string) ([]prefetchImage, error) {
	var rep prefetchReport
	if err := json.NewDecoder(r).Decode(&rep); err != nil {
		return nil, fmt.Errorf("not a report of admin repo prefetch: %w", err)
	}
	var images []prefetchImage
	for _, res := range rep.Results {
		if res.Status == proxy.PrefetchFailed {
			images = append(images, prefetchImage{ref: res.Name, platforms: cmpOr(rep.Platforms[res.Name], platforms)})
		}
	}
	return images, nil
}

// imageLines reads the lines of a file of images, <reference> [<platform>,…], with platforms for
// a line that names none; with host, those of other registries are skipped.
func imageLines(r io.Reader, platforms []string, host string) (images []prefetchImage, skipped []string, err error) {
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
			continue
		case len(fields) > 2:
			return nil, nil, fmt.Errorf("line %d: %d fields; a line is <reference> [<platform>,…]", n, len(fields))
		}
		im := prefetchImage{ref: fields[0], platforms: platforms}
		if len(fields) == 2 {
			im.platforms = strings.Split(fields[1], ",")
			if err := checkPlatforms(im.platforms); err != nil {
				return nil, nil, fmt.Errorf("line %d: %w", n, err)
			}
		}
		if host != "" {
			ref, ok := ofHost(im.ref, host)
			if !ok {
				skipped = append(skipped, im.ref)
				continue
			}
			im.ref = ref
		}
		images = append(images, im)
	}
	return images, skipped, sc.Err()
}

// groupImages groups images by their platforms, each reference once with the platforms of its
// first line, in the order of each group's first image.
func groupImages(images []prefetchImage) []prefetchGroup {
	var groups []prefetchGroup
	seen, at := map[string]bool{}, map[string]int{}
	for _, im := range images {
		if seen[im.ref] {
			continue
		}
		seen[im.ref] = true
		key := strings.Join(im.platforms, ",")
		i, ok := at[key]
		if !ok {
			i, at[key] = len(groups), len(groups)
			groups = append(groups, prefetchGroup{platforms: im.platforms})
		}
		groups[i].names = append(groups[i].names, im.ref)
	}
	return groups
}

// cmpOr is a when it is not empty, else b.
func cmpOr(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

// checkPlatforms reports the first of platforms that is not os/arch[/variant].
func checkPlatforms(platforms []string) error {
	for _, pl := range platforms {
		parts := strings.Split(pl, "/")
		if len(parts) < 2 || len(parts) > 3 || slices.Contains(parts, "") {
			return fmt.Errorf("%q is not <os>/<architecture>[/<variant>]", pl)
		}
	}
	return nil
}

// dockerHubHosts are the names of Docker Hub's registry, which a reference without a registry
// names.
var dockerHubHosts = []string{"docker.io", "index.docker.io", "registry-1.docker.io"}

// ofHost returns ref without its registry when its registry is host, and whether it is: the
// first component of a reference of several is its registry when it has a dot or a colon or is
// localhost, as docker reads it, and a reference without one is Docker Hub's.
func ofHost(ref, host string) (string, bool) {
	reg, rest := "docker.io", ref
	if first, after, more := strings.Cut(ref, "/"); more && (strings.ContainsAny(first, ".:") || first == "localhost") {
		reg, rest = first, after
	}
	same := reg == host || slices.Contains(dockerHubHosts, reg) && slices.Contains(dockerHubHosts, host)
	return rest, same
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
