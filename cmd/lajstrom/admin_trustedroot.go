package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/spf13/cobra"
)

// trustedRootTarget is the TUF target of the trusted root of a Sigstore instance.
const trustedRootTarget = "trusted_root.json"

// newAdminTrustedRootCmd returns admin trusted-root: the trusted root of keyless attachments
// (#89; docs/spec/manifest-profile.md §6.3), without a configuration or a server.
func newAdminTrustedRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trusted-root",
		Short: "Fetch the trusted root that keyless attachments verify against",
	}
	cmd.AddCommand(newTrustedRootFetchCmd(fetchTrustedRoot))
	return cmd
}

// fetchTrustedRoot returns the trusted_root.json of the TUF repository of opts, verified by TUF
// from opts.Root: the network request of the command, and the only one of keyless verification.
func fetchTrustedRoot(opts *tuf.Options) ([]byte, error) {
	c, err := tuf.New(opts)
	if err != nil {
		return nil, fmt.Errorf("the TUF repository %s: %w", opts.RepositoryBaseURL, err)
	}
	b, err := c.GetTarget(trustedRootTarget)
	if err != nil {
		return nil, fmt.Errorf("%s of %s: %w", trustedRootTarget, opts.RepositoryBaseURL, err)
	}
	return b, nil
}

// newTrustedRootFetchCmd returns admin trusted-root fetch, which fetches with fetch.
func newTrustedRootFetchCmd(fetch func(*tuf.Options) ([]byte, error)) *cobra.Command {
	var out, tufURL, tufRoot string
	cmd := &cobra.Command{
		Use:   "fetch --out <file> [--tuf-url <url> --tuf-root <root.json>]",
		Short: "Fetch a Sigstore trusted_root.json through TUF, check it and write it, for attachments.keyless.trustedRoot",
		Long: "Fetch the trusted_root.json of a Sigstore instance through TUF, verified from its TUF root: the\n" +
			"public-good instance (" + tuf.DefaultMirror + ") from the root sigstore-go embeds, or another one\n" +
			"with --tuf-url and its root.json with --tuf-root. The file is parsed before it is written, and\n" +
			"written whole or not at all. Lajstrom verifies keyless attachments offline against the file of\n" +
			"attachments.keyless.trustedRoot: run this when the instance's keys rotate, then restart serve and\n" +
			"reindex. Nothing is cached in the home directory.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out == "" {
				return errors.New("--out: the file to write")
			}
			opts := tuf.DefaultOptions().WithDisableLocalCache().WithContext(cmd.Context())
			if tufURL != "" {
				if tufRoot == "" {
					return errors.New("--tuf-root: the root.json of the TUF repository of --tuf-url")
				}
				b, err := os.ReadFile(tufRoot) //nolint:gosec // the operator names the file
				if err != nil {
					return fmt.Errorf("--tuf-root: %w", err)
				}
				opts = opts.WithRepositoryBaseURL(tufURL).WithRoot(b)
			}
			b, err := fetch(opts)
			if err != nil {
				return err
			}
			tr, err := root.NewTrustedRootFromJSON(b)
			if err != nil {
				return fmt.Errorf("%s of %s is not a trusted root: %w", trustedRootTarget, opts.RepositoryBaseURL, err)
			}
			if err := writeWhole(out, b); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s from %s: %d certificate authorities, %d transparency logs, %d CT logs, %d timestamp authorities\n",
				out, opts.RepositoryBaseURL, len(tr.FulcioCertificateAuthorities()), len(tr.RekorLogs()), len(tr.CTLogs()), len(tr.TimestampingAuthorities()))
			return err
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "the file to write: attachments.keyless.trustedRoot")
	cmd.Flags().StringVar(&tufURL, "tuf-url", "", "the TUF repository of another Sigstore instance; default "+tuf.DefaultMirror)
	cmd.Flags().StringVar(&tufRoot, "tuf-root", "", "the root.json of the TUF repository of --tuf-url")
	return cmd
}

// writeWhole writes b to path through a file beside it, renamed over it once written: a reader
// finds the old file or the new one, never a part.
func writeWhole(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
