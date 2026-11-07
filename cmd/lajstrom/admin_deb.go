package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/format/deb"
)

// newAdminDebCmd returns admin deb: what an oras push of a .deb to an apt repo needs from the
// package, without a configuration or a server.
func newAdminDebCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deb",
		Short: "Read .deb packages for an oras push to an apt repo",
	}
	cmd.AddCommand(newDebMetadataCmd())
	return cmd
}

// newDebMetadataCmd returns admin deb metadata: print the deb metadata layer of a package
// (docs/spec/manifest-profile.md §2), the bytes the apt upload route writes for it, so that an
// oras push by hand carries the mandatory metadata layer without computing it in shell. One
// read of the file, deb.Parse.
func newDebMetadataCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "metadata <file.deb>",
		Short: "Print the metadata layer of a .deb, the metadata.json of an oras push to an apt repo",
		Long: "Print the deb metadata layer of a .deb: the control fields, md5sums, and the size, md5, sha1\n" +
			"and sha256 of the file and its pool file name, as compact JSON without a trailing newline,\n" +
			"the bytes PUT /apt/<repo>/upload writes. Push it as the layer\n" +
			"metadata.json:application/vnd.lajstrom.deb.metadata.v1+json (docs/quickstart/apt.md).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, f.Close()) }()
			pkg, err := deb.Parse(f)
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
