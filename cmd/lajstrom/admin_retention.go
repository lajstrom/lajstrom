package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
)

// The environment of admin retention: the server and its credential, so that a CronJob passes
// them as a Secret.
const (
	envRetentionURL       = "LAJSTROM_URL"
	envRetentionToken     = "LAJSTROM_TOKEN"      //nolint:gosec // the name of a variable, not a credential
	envRetentionTokenFile = "LAJSTROM_TOKEN_FILE" //nolint:gosec // the name of a variable, not a credential
)

// retentionBatch is how many versions one request of --apply sends at most, and
// retentionBatchBytes how many bytes of JSON their items take at most, under the 64 KiB the API
// reads of a body: versions of many variants go in smaller batches.
const (
	retentionBatch      = 10
	retentionBatchBytes = 48 << 10
)

// retentionTimeout bounds a request of admin retention that lists, and retentionItemTimeout
// each version of a batch of --apply: the server deletes them one after another, with about 7
// registry requests for each variant, so a batch of n versions may take n times
// retentionItemTimeout.
const (
	retentionTimeout     = 2 * time.Minute
	retentionItemTimeout = 2 * time.Minute
)

// exitCode is an error that is only an exit status: main prints nothing for it. A dry run of
// admin retention that finds versions to delete ends with exitCode(2).
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// retentionRules are the rules of a run: a version is kept when any of them keeps it.
type retentionRules struct {
	Packages     []string `json:"packages,omitempty"` // globs of path.Match; none: every package
	KeepLast     *int     `json:"keepLast,omitempty"` // the newest N by publish time
	MaxAge       string   `json:"maxAge,omitempty"`   // younger than it, by the newest variant
	DeleteNewest bool     `json:"deleteNewest"`       // the newest version may go too

	maxAge time.Duration
}

// retentionVersion is a version that the rules leave, and why.
type retentionVersion struct {
	Package      string    `json:"package"`
	Version      string    `json:"version"`
	Digests      []string  `json:"digests"`
	PublishedAt  time.Time `json:"publishedAt"`
	LastModified time.Time `json:"lastModified"`
	AgeSeconds   int64     `json:"ageSeconds"`
	Rank         int       `json:"rank"`
	Reasons      []string  `json:"reasons"`
}

// retentionReport is what a run decided and, with --apply, what the server did: the JSON of
// --output json. Error is the error that stopped --apply at a batch: the results are of the
// batches before it, and the server may have deleted versions of that batch.
type retentionReport struct {
	Repo     string                `json:"repo"`
	DryRun   bool                  `json:"dryRun"`
	Now      time.Time             `json:"now"`
	Cutoff   time.Time             `json:"cutoff"`
	Rules    retentionRules        `json:"rules"`
	Packages int                   `json:"packages"`
	Versions int                   `json:"versions"`
	Delete   []retentionVersion    `json:"delete"`
	Job      string                `json:"job,omitempty"`
	Results  []api.BatchItemResult `json:"results,omitempty"`
	Error    string                `json:"error,omitempty"`
}

// newAdminRetentionCmd returns admin retention: a client of the management API that deletes the
// versions of a repo its rules leave (docs/operations.md, Retention). It lists the packages and
// their versions, newest first by publish time, and keeps a version that --keep-last or
// --max-age keeps, that a pointer (an npm dist-tag) names, or that is the newest of its package
// (unless --delete-newest). By default it is a dry run that prints the versions it would delete
// and why; --apply sends them as batches of one job, POST …/versions:delete, with the digests it
// read and the cut-off it decided on, and the server deletes each only if it has not changed
// meanwhile; the text output has the results of each batch as the server answers it. SIGINT or
// SIGTERM stops the run, which then prints what the server answered. The server is --url or
// LAJSTROM_URL, the credential a personal access token from --token-file, LAJSTROM_TOKEN_FILE
// or LAJSTROM_TOKEN; the configuration is not read. The time is the server's, from the Date of
// its first answer. Exit status 0: nothing to delete, or deleted; 2: a dry run found versions
// to delete; 1: an error, or an item the server failed.
func newAdminRetentionCmd(a *app) *cobra.Command {
	var (
		repo, maxAge, output, base, tokenFile string
		keepLast                              int
		rules                                 retentionRules
		apply                                 bool
	)
	cmd := &cobra.Command{
		Use: "retention --repo <format>/<name> [--package <glob>]... [--keep-last N] [--max-age 90d] [--delete-newest] " +
			"[--apply] [--output text|json]",
		Short: "Delete the versions of a repo that the rules leave, through the server's API; a dry run unless --apply",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, name, ok := strings.Cut(repo, "/")
			if !ok || f == "" || name == "" || strings.Contains(name, "/") {
				return fmt.Errorf("--repo: %q is not <format>/<name>", repo)
			}
			if output != "text" && output != "json" {
				return fmt.Errorf("--output: %q is not text or json", output)
			}
			if cmd.Flags().Changed("keep-last") {
				rules.KeepLast = &keepLast
			}
			if err := rules.check(maxAge); err != nil {
				return err
			}
			c, err := newRetentionClient(a.lookupEnv, base, tokenFile)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// The first signal stops the run, which prints what it has; a second one the process.
			context.AfterFunc(ctx, stop)
			r := &retention{client: c, format: f, name: name, rules: rules, out: cmd.OutOrStdout(), json: output == "json"}
			return r.run(ctx, apply)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&repo, "repo", "", "the repo, <format>/<name>")
	fl.StringArrayVar(&rules.Packages, "package", nil, "only the packages whose name matches the glob (path.Match: * does not match /); again for more")
	fl.IntVar(&keepLast, "keep-last", 0, "keep the N newest versions of each package, by publish time")
	fl.StringVar(&maxAge, "max-age", "", "keep the versions younger than this, by their newest variant: 90d, 36h")
	fl.BoolVar(&rules.DeleteNewest, "delete-newest", false, "the newest version of a package may be deleted too")
	fl.BoolVar(&apply, "apply", false, "delete; without it the run prints what it would delete")
	fl.StringVar(&output, "output", "text", "text or json")
	fl.StringVar(&base, "url", "", "the server's base URL (env "+envRetentionURL+")")
	fl.StringVar(&tokenFile, "token-file", "", "a file with a personal access token (env "+envRetentionTokenFile+", or the token in "+envRetentionToken+")")
	_ = cmd.MarkFlagRequired("repo")
	return cmd
}

// check checks the rules of the flags and sets the duration of maxAge, the flag --max-age.
func (rules *retentionRules) check(maxAge string) error {
	if rules.KeepLast != nil && *rules.KeepLast < 0 {
		return fmt.Errorf("--keep-last: %d is negative", *rules.KeepLast)
	}
	if maxAge != "" {
		d, err := auth.ParseLifetime(maxAge)
		if err != nil {
			return fmt.Errorf("--max-age: %w", err)
		}
		rules.MaxAge, rules.maxAge = maxAge, d
	}
	if rules.KeepLast == nil && rules.maxAge == 0 {
		return errors.New("name a rule: --keep-last, --max-age or both")
	}
	for _, g := range rules.Packages {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("--package: %q is not a glob: %w", g, err)
		}
	}
	return nil
}

// retentionClient is the HTTP client of admin retention. A request is bounded by the timeout
// that do is given, not by the client: timeout for a list, itemTimeout for each version of a
// batch (retentionTimeout, retentionItemTimeout; the tests shorten them).
type retentionClient struct {
	base                 string // the server, without a trailing /
	token                string
	hc                   *http.Client
	timeout, itemTimeout time.Duration
	now                  time.Time // the server's time, from the Date of its first answer
}

// newRetentionClient returns the client of the server of flag or LAJSTROM_URL with the token of
// the file of flag, of LAJSTROM_TOKEN_FILE, or of LAJSTROM_TOKEN.
func newRetentionClient(lookupEnv func(string) (string, bool), base, tokenFile string) (*retentionClient, error) {
	env := func(k string) string { v, _ := lookupEnv(k); return v }
	if base == "" {
		base = env(envRetentionURL)
	}
	u, err := url.Parse(base)
	if base == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("the server: %q is not an http or https URL (--url or %s)", base, envRetentionURL)
	}
	if tokenFile == "" {
		tokenFile = env(envRetentionTokenFile)
	}
	token := env(envRetentionToken)
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile) //nolint:gosec // the operator names the file
		if err != nil {
			return nil, fmt.Errorf("the token: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		return nil, fmt.Errorf("the token: none in --token-file, %s or %s", envRetentionTokenFile, envRetentionToken)
	}
	return &retentionClient{base: strings.TrimSuffix(base, "/"), token: token, hc: &http.Client{}, timeout: retentionTimeout,
		itemTimeout: retentionItemTimeout}, nil
}

// do sends a request of method to target, below the base, with the JSON of body when it is not
// nil, and decodes the JSON answer into v, all within timeout. An answer that is not 2xx is an
// error with the server's message.
func (c *retentionClient) do(ctx context.Context, timeout time.Duration, method, target string, body, v any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+target, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if c.now.IsZero() {
		if c.now, err = http.ParseTime(resp.Header.Get("Date")); err != nil {
			c.now = time.Now().UTC().Truncate(time.Second)
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, target, err)
	}
	if resp.StatusCode/100 != 2 {
		var env struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &env) == nil && env.Error.Message != "" {
			msg = env.Error.Code + ": " + env.Error.Message
		}
		return fmt.Errorf("%s %s: %s: %s", method, target, resp.Status, msg)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s %s: the answer: %w", method, target, err)
	}
	return nil
}

// retention is one run.
type retention struct {
	client       *retentionClient
	format, name string
	rules        retentionRules
	out          io.Writer
	json         bool
}

// repoPath is the API path of the repo.
func (r *retention) repoPath() string {
	return "/api/v1/repos/" + url.PathEscape(r.format) + "/" + url.PathEscape(r.name)
}

// run lists, decides, and with apply deletes, then reports: in text, the results of each batch
// as the server answers it; a run that an error stops prints what it has, then returns the
// error.
func (r *retention) run(ctx context.Context, apply bool) error {
	rep := retentionReport{Repo: r.format + "/" + r.name, DryRun: !apply, Rules: r.rules, Delete: []retentionVersion{}}
	pkgs, err := r.packages(ctx)
	if err != nil {
		return err
	}
	rep.Now = r.client.now
	rep.Cutoff = rep.Now
	if r.rules.maxAge > 0 {
		rep.Cutoff = rep.Now.Add(-r.rules.maxAge)
	}
	for _, p := range pkgs {
		versions, err := r.versions(ctx, p)
		if err != nil {
			return err
		}
		rep.Packages++
		rep.Versions += len(versions)
		rep.Delete = append(rep.Delete, r.rules.decide(p, versions, rep.Now)...)
	}
	deleting := apply && len(rep.Delete) > 0
	if deleting {
		var nonce [4]byte
		_, _ = rand.Read(nonce[:])
		rep.Job = "retention-" + rep.Now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(nonce[:])
	}
	if err := r.printHead(rep); err != nil {
		return err
	}
	if deleting {
		if err := r.apply(ctx, &rep); err != nil {
			rep.Error = err.Error()
			return errors.Join(err, r.printTail(rep))
		}
	}
	if err := r.printTail(rep); err != nil {
		return err
	}
	for _, res := range rep.Results {
		if res.Status == "error" {
			return fmt.Errorf("the server failed to delete %s %s: %s", res.Package, res.Version, res.Message)
		}
	}
	if !apply && len(rep.Delete) > 0 {
		return exitCode(2)
	}
	return nil
}

// packages returns the names of the repo's packages that the rules' globs match.
func (r *retention) packages(ctx context.Context) ([]string, error) {
	var names []string
	q := url.Values{"limit": {"1000"}}
	if p := literalPrefix(r.rules.Packages); p != "" {
		q.Set("prefix", p)
	}
	for {
		var page api.PackageList
		if err := r.client.do(ctx, r.client.timeout, http.MethodGet, r.repoPath()+"/packages?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, p := range page.Packages {
			if r.rules.matches(p.Name) {
				names = append(names, p.Name)
			}
		}
		if page.Next == "" {
			return names, nil
		}
		q.Set("cursor", page.Next)
	}
}

// literalPrefix is the prefix that every name the globs match starts with: the part of a single
// glob before its first special character, or "" for none or several globs.
func literalPrefix(globs []string) string {
	if len(globs) != 1 {
		return ""
	}
	if i := strings.IndexAny(globs[0], `*?[\`); i >= 0 {
		return globs[0][:i]
	}
	return globs[0]
}

// matches reports whether a rule's glob matches the package name; with none, every name does.
func (rules retentionRules) matches(name string) bool {
	if len(rules.Packages) == 0 {
		return true
	}
	for _, g := range rules.Packages {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}

// versions returns the versions of the package pkg, newest first.
func (r *retention) versions(ctx context.Context, pkg string) ([]api.VersionEntry, error) {
	var all []api.VersionEntry
	q := url.Values{"limit": {"1000"}}
	for {
		var page api.VersionList
		target := r.repoPath() + "/packages/" + url.PathEscape(pkg) + "/versions?" + q.Encode()
		if err := r.client.do(ctx, r.client.timeout, http.MethodGet, target, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Versions...)
		if page.Next == "" {
			return all, nil
		}
		q.Set("cursor", page.Next)
	}
}

// decide returns the versions of pkg, newest first as the list has them, that no rule keeps, at
// the server's time now. Its rank is its place in the list, 1 the newest; its age is that of
// its newest variant.
func (rules retentionRules) decide(pkg string, versions []api.VersionEntry, now time.Time) []retentionVersion {
	var out []retentionVersion
	for i, v := range versions {
		rank, age := i+1, now.Sub(v.LastModified)
		switch {
		case len(v.Pointers) > 0, rank == 1 && !rules.DeleteNewest:
			continue
		case rules.KeepLast != nil && rank <= *rules.KeepLast, rules.maxAge > 0 && age < rules.maxAge:
			continue
		}
		rv := retentionVersion{Package: pkg, Version: v.Version, PublishedAt: v.PublishedAt, LastModified: v.LastModified,
			AgeSeconds: int64(age / time.Second), Rank: rank}
		for _, variant := range v.Variants {
			rv.Digests = append(rv.Digests, variant.Digest)
		}
		if rules.KeepLast != nil {
			rv.Reasons = append(rv.Reasons, fmt.Sprintf("rank %d > keep-last %d", rank, *rules.KeepLast))
		}
		if rules.maxAge > 0 {
			rv.Reasons = append(rv.Reasons, fmt.Sprintf("age %s > max-age %s", days(age), rules.MaxAge))
		}
		out = append(out, rv)
	}
	return out
}

// days is a duration in whole days, or hours below a day.
func days(d time.Duration) string {
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// apply sends the versions of rep to the server as batches of its job (retentionBatches), with
// the cut-off, keeps what the server did with each, and prints the results of each batch as
// they arrive (printResults). A batch's request may take the client's itemTimeout for each of
// its versions. An error stops the run at its batch: the server may have deleted versions of
// it, and is sent none of the batches after it.
func (r *retention) apply(ctx context.Context, rep *retentionReport) error {
	items := make([]api.BatchDeleteItem, len(rep.Delete))
	for i, v := range rep.Delete {
		items[i] = api.BatchDeleteItem{Package: v.Package, Version: v.Version, Digests: v.Digests}
	}
	sent := 0
	for _, batch := range retentionBatches(items) {
		req := api.BatchDeleteRequest{Cutoff: rep.Cutoff.UTC().Format(time.RFC3339), Job: rep.Job, Items: batch}
		var res api.BatchDeleteResult
		timeout := time.Duration(len(batch)) * r.client.itemTimeout
		if err := r.client.do(ctx, timeout, http.MethodPost, r.repoPath()+"/versions:delete", req, &res); err != nil {
			return fmt.Errorf("the batch of %d versions from %s %s: %w; the %d versions after it were not sent",
				len(batch), batch[0].Package, batch[0].Version, err, len(items)-sent-len(batch))
		}
		sent += len(batch)
		rep.Results = append(rep.Results, res.Results...)
		if err := r.printResults(res.Results); err != nil {
			return err
		}
	}
	return nil
}

// retentionBatches splits items into batches of at most retentionBatch items whose JSON takes
// at most retentionBatchBytes; an item larger than that is a batch of its own.
func retentionBatches(items []api.BatchDeleteItem) [][]api.BatchDeleteItem {
	var out [][]api.BatchDeleteItem
	start, size := 0, 0
	for i, it := range items {
		b, _ := json.Marshal(it)
		if i > start && (i-start == retentionBatch || size+len(b)+1 > retentionBatchBytes) {
			out = append(out, items[start:i])
			start, size = i, 0
		}
		size += len(b) + 1
	}
	if start < len(items) {
		out = append(out, items[start:])
	}
	return out
}

// printHead writes the first lines of the text report: what the run decided and, in a dry run,
// each version it would delete and why. The JSON document is written whole, by printTail.
func (r *retention) printHead(rep retentionReport) error {
	if r.json {
		return nil
	}
	var b strings.Builder
	what := "to delete (a dry run; --apply deletes them)"
	if !rep.DryRun {
		what = "sent to the server as job " + rep.Job
		if len(rep.Delete) == 0 {
			what = "to delete"
		}
	}
	fmt.Fprintf(&b, "%s: %d of %d versions in %d packages %s; cut-off %s\n", rep.Repo, len(rep.Delete), rep.Versions, rep.Packages,
		what, rep.Cutoff.UTC().Format(time.RFC3339))
	if rep.DryRun {
		for _, v := range rep.Delete {
			fmt.Fprintf(&b, "delete %s %s  published %s  %s\n", v.Package, v.Version, v.PublishedAt.UTC().Format(time.RFC3339),
				strings.Join(v.Reasons, ", "))
		}
	}
	_, err := io.WriteString(r.out, b.String())
	return err
}

// printResults writes a line of text for each result of a batch.
func (r *retention) printResults(results []api.BatchItemResult) error {
	if r.json {
		return nil
	}
	var b strings.Builder
	for _, res := range results {
		line := fmt.Sprintf("%-9s %s %s", res.Status, res.Package, res.Version)
		if res.Message != "" {
			line += ": " + res.Message
		}
		fmt.Fprintln(&b, line)
	}
	_, err := io.WriteString(r.out, b.String())
	return err
}

// printTail writes the end of the report: the JSON document, or after --apply the counts of
// the results, of those the server answered when an error stopped the run.
func (r *retention) printTail(rep retentionReport) error {
	if r.json {
		enc := json.NewEncoder(r.out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if rep.DryRun || len(rep.Delete) == 0 {
		return nil
	}
	counts := map[string]int{}
	for _, res := range rep.Results {
		counts[res.Status]++
	}
	_, err := fmt.Fprintf(r.out, "deleted %d, changed %d, protected %d, young %d, gone %d, error %d\n", counts["deleted"],
		counts["changed"], counts["protected"], counts["young"], counts["gone"], counts["error"])
	return err
}
