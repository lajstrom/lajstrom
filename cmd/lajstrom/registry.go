package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider/ecr"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
	"github.com/lajstrom/lajstrom/internal/provider/probe"
)

// newRegistryCmd returns the registry group: probe.
func newRegistryCmd(a *app) *cobra.Command {
	reg := &cobra.Command{
		Use:   "registry",
		Short: "Measure a backend registry",
	}
	reg.AddCommand(newRegistryProbeCmd(a))
	return reg
}

// probeFlags are the flags of registry probe.
type probeFlags struct {
	registries   []string
	urls         []string
	username     string
	passwordFile string
	prefix       string
	scratch      string
	maxBlob      string
	maxManifest  string
	routingHeads int
	output       string
	verbose      bool
	timeout      time.Duration
}

// newRegistryProbeCmd returns registry probe: the row of the capability matrix
// (docs/registries/README.md) of each registry named, measured in a scratch prefix that the
// probe creates and empties again.
func newRegistryProbeCmd(a *app) *cobra.Command {
	var f probeFlags
	cmd := &cobra.Command{
		Use:   "probe",
		Short: "Measure the capability matrix row of registries of the configuration (--registry) or named by URL (--url)",
		Long: "probe pushes small manifests and blobs below a new scratch prefix of each registry, measures every\n" +
			"cell of the capability matrix (docs/registries/README.md) it can, deletes what it pushed, and\n" +
			"writes the cells: a Markdown table, one column per registry, or JSON with --output json; -v adds\n" +
			"the requests and answers of each cell. It refuses a scratch prefix that holds anything.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			targets, err := a.probeTargets(f)
			if err != nil {
				return err
			}
			opts, err := f.options(a)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()
			var reports []*probe.Report
			for _, t := range targets {
				r, err := probe.Run(ctx, t, opts)
				if err != nil {
					return fmt.Errorf("registry %q: %w", t.Name, err)
				}
				reports = append(reports, r)
			}
			if f.output == "json" {
				return probe.WriteJSON(cmd.OutOrStdout(), reports, f.verbose)
			}
			return probe.WriteMarkdown(cmd.OutOrStdout(), reports, f.verbose)
		},
	}
	fl := cmd.Flags()
	fl.StringSliceVar(&f.registries, "registry", nil, "registries[].name of the configuration (--config) to probe; repeat for several")
	fl.StringSliceVar(&f.urls, "url", nil, "base URL of a registry to probe without a configuration, e.g. http://localhost:5000; repeat for several")
	fl.StringVar(&f.username, "username", "", "with --url: the user name for the registry")
	fl.StringVar(&f.passwordFile, "password-file", "", "with --url: a file whose content is the password or token")
	fl.StringVar(&f.prefix, "registry-prefix", "", "with --url: the instance prefix, as registries[].prefix (a Harbor project, a Gitea owner)")
	fl.StringVar(&f.scratch, "scratch", "", "the scratch prefix below the instance prefix; it must not hold anything (default lajstrom-probe-<random>)")
	fl.StringVar(&f.maxBlob, "max-blob-size", "8MiB", "the largest blob pushed: bytes, or with KiB, MiB or GiB")
	fl.StringVar(&f.maxManifest, "max-manifest-size", strconv.Itoa(probe.DefaultMaxManifestSize), "the bound of the manifest size ladder: bytes, or with KiB, MiB or GiB")
	fl.IntVar(&f.routingHeads, "routing-heads", probe.DefaultRoutingHeads, "how many times the row \"Routing words in a name\" asks for each blob by HEAD")
	fl.StringVarP(&f.output, "output", "o", "markdown", "markdown or json")
	fl.BoolVarP(&f.verbose, "verbose", "v", false, "add the requests and the trimmed answers of every cell")
	fl.DurationVar(&f.timeout, "timeout", 15*time.Minute, "bound on the whole run")
	return cmd
}

// probeTargets returns the registries to probe: those of the configuration that --registry
// names, then one per --url.
func (a *app) probeTargets(f probeFlags) ([]config.Registry, error) {
	if len(f.registries) == 0 && len(f.urls) == 0 {
		return nil, errors.New("name a registry with --registry (of the configuration) or --url")
	}
	if len(f.registries) > 0 && (f.username != "" || f.passwordFile != "" || f.prefix != "") {
		return nil, errors.New("--username, --password-file and --registry-prefix go with --url; --registry takes them from the configuration")
	}
	var out []config.Registry
	if len(f.registries) > 0 {
		cfg, err := config.Load(a.configPath, a.lookupEnv)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, configError{fmt.Errorf("%w (choose the file with --config or LAJSTROM_CONFIG, or name the registry with --url)", err)}
		}
		if err != nil {
			return nil, configError{err}
		}
		for _, name := range f.registries {
			r, ok := registryNamed(cfg.Registries, name)
			if !ok {
				return nil, fmt.Errorf("no registry %q in %s", name, a.configPath)
			}
			out = append(out, r)
		}
	}
	for _, raw := range f.urls {
		r, err := f.urlRegistry(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// registryNamed returns the registry of the configuration named name.
func registryNamed(rs []config.Registry, name string) (config.Registry, bool) {
	for _, r := range rs {
		if r.Name == name {
			return r, true
		}
	}
	return config.Registry{}, false
}

// urlRegistry is the registry of --url raw, with the credentials and the prefix of the flags;
// its name is the URL's host.
func (f probeFlags) urlRegistry(raw string) (config.Registry, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return config.Registry{}, fmt.Errorf("--url %q is not an absolute URL", raw)
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return config.Registry{}, errors.New("--url must be the registry's base URL, http(s)://<host>[:port], with no path, " +
			"query or fragment: the API is at /v2/ of the host, and a project or an owner goes in --registry-prefix")
	}
	r := config.Registry{Name: u.Host, Type: generic.Type, URL: raw, Prefix: f.prefix}
	if ecr.IsRegistryHost(u.Hostname()) {
		// The authorization token of the ECR API, for the AWS SDK's credentials; the region is
		// the host's.
		if f.username != "" {
			return config.Registry{}, errors.New("--username does not go with an ECR registry: the probe asks the ECR API for a token " +
				"with the AWS SDK's credentials (AWS_PROFILE, AWS_ACCESS_KEY_ID, …)")
		}
		r.Type = ecr.Type
		return r, nil
	}
	if f.username != "" {
		if f.passwordFile == "" {
			return config.Registry{}, errors.New("--username needs --password-file")
		}
		b, err := os.ReadFile(f.passwordFile) // #nosec G304 -- the operator names the file
		if err != nil {
			return config.Registry{}, fmt.Errorf("--password-file: %w", err)
		}
		r.Auth = config.RegistryAuth{Username: f.username, Password: strings.TrimRight(string(b), "\r\n")}
	}
	return r, nil
}

// options are the probe's options from the flags.
func (f probeFlags) options(a *app) (probe.Options, error) {
	if f.output != "markdown" && f.output != "json" {
		return probe.Options{}, fmt.Errorf("--output %q is not markdown or json", f.output)
	}
	blob, err := parseSize(f.maxBlob)
	if err != nil {
		return probe.Options{}, fmt.Errorf("--max-blob-size: %w", err)
	}
	man, err := parseSize(f.maxManifest)
	if err != nil {
		return probe.Options{}, fmt.Errorf("--max-manifest-size: %w", err)
	}
	if f.routingHeads < 1 {
		return probe.Options{}, fmt.Errorf("--routing-heads %d: at least 1", f.routingHeads)
	}
	return probe.Options{Scratch: f.scratch, MaxBlobSize: blob, MaxManifestSize: man, RoutingHeads: f.routingHeads, Log: a.log}, nil
}

// parseSize reads a size: a number of bytes, or one followed by KiB, MiB or GiB.
func parseSize(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"", 1}}
	for _, u := range units {
		num, ok := strings.CutSuffix(s, u.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
		if err != nil || n <= 0 || n > (1<<62)/u.mult {
			return 0, fmt.Errorf("%q is not a positive size in bytes, KiB, MiB or GiB", s)
		}
		return n * u.mult, nil
	}
	return 0, fmt.Errorf("%q is not a size", s)
}
