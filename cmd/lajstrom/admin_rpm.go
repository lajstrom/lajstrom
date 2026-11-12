package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/format/rpm"
)

// newAdminRpmCmd returns admin rpm: what an oras push of an .rpm to an rpm repo needs from the
// package, without a configuration or a server.
func newAdminRpmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rpm",
		Short: "Read .rpm packages for an oras push to an rpm repo",
	}
	cmd.AddCommand(newRpmMetadataCmd())
	return cmd
}

// newRpmMetadataCmd returns admin rpm metadata: print the rpm metadata layer of a package
// (docs/spec/manifest-profile.md §2), the bytes the rpm upload route writes for it, so that an
// oras push by hand carries the mandatory metadata layer without computing it in shell. One
// read of the file, rpm.Parse.
func newRpmMetadataCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "metadata <file.rpm>",
		Short: "Print the metadata layer of an .rpm, the metadata.json of an oras push to an rpm repo",
		Long: "Print the rpm metadata layer of an .rpm: what the repodata needs from its header (names,\n" +
			"relations, files, changelog) and the size and sha256 of the file and its file name, as compact\n" +
			"JSON without a trailing newline, the bytes PUT /rpm/<repo>/upload writes. Push it as the layer\n" +
			"metadata.json:application/vnd.lajstrom.rpm.metadata.v1+json (docs/spec/http-routing.md, rpm).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, f.Close()) }()
			pkg, err := rpm.Parse(f)
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			md, err := pkg.Metadata()
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			_, err = cmd.OutOrStdout().Write(md)
			return err
		},
	}
}
