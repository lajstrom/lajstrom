package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// aptFormat is the format whose repos have signing keys: the apt plugin (internal/format/deb).
const aptFormat = "apt"

// newAdminKeyCmd returns admin key: export and rotate the signing key of an apt repo in the
// index database, without HTTP. Both need the master key, from the variable keys.masterKeyEnv
// names, and assume that lajstrom migrate ran.
func newAdminKeyCmd(a *app) *cobra.Command {
	key := &cobra.Command{
		Use:   "key",
		Short: "Export and rotate the signing keys of apt repos",
	}
	key.AddCommand(newKeyExportCmd(a), newKeyRotateCmd(a))
	return key
}

// newKeyExportCmd returns admin key export: print the armored keyring of an apt repo, what
// apt's signed-by= points at, the same bytes as GET /apt/<name>/key.asc. A repo without a key
// gets it now.
func newKeyExportCmd(a *app) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:         "export --repo apt/<name>",
		Short:       "Print the armored keyring of an apt repo (signed-by=); makes the key when there is none",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withKeys(cmd.Context(), repo, func(ctx context.Context, svc *keys.Service, r index.Repo) error {
				b, err := svc.PublicKeys(ctx, r)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(b)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "the apt repo, apt/<name>")
	_ = cmd.MarkFlagRequired("repo") // fails only for a flag that does not exist
	return cmd
}

// newKeyRotateCmd returns admin key rotate: give an apt repo a new signing key, retire the
// old one (it signs beside the new one and stays in key.asc for keys.retireGrace), drop the
// repo's cached indexes, and print the new fingerprint.
func newKeyRotateCmd(a *app) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:         "rotate --repo apt/<name>",
		Short:       "Give an apt repo a new signing key and print its fingerprint",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withKeys(cmd.Context(), repo, func(ctx context.Context, svc *keys.Service, r index.Repo) error {
				k, err := svc.Rotate(ctx, r)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), k.Key.Fingerprint)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "the apt repo, apt/<name>")
	_ = cmd.MarkFlagRequired("repo")
	return cmd
}

// withKeys loads the master key, opens the index database, looks up the apt repo arg and runs
// fn with the keys service over the database, then closes it.
func (a *app) withKeys(ctx context.Context, arg string, fn func(context.Context, *keys.Service, index.Repo) error) (err error) {
	formatName, name, err := splitRepo(arg)
	if err != nil {
		return err
	}
	if formatName != aptFormat {
		return fmt.Errorf("repo %q: only apt repos have signing keys", arg)
	}
	m, err := keys.LoadMaster(a.cfg.Keys.MasterKeyEnv, a.lookupEnv)
	if err != nil {
		return fmt.Errorf("admin key needs the master key: %w", err)
	}
	st, err := openStore(ctx, a.cfg.Database)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	repo, err := st.GetRepo(ctx, formatName, name)
	if errors.Is(err, index.ErrNotFound) {
		return fmt.Errorf("repo %s does not exist", arg)
	}
	if err != nil {
		return err
	}
	svc, err := newKeys(a.cfg, st, m, a.log)
	if err != nil {
		return err
	}
	return fn(ctx, svc, *repo)
}

// newKeys returns the keys service of the configuration over st.
func newKeys(c *config.Config, st index.Store, m *keys.Master, log *slog.Logger) (*keys.Service, error) {
	return keys.New(keys.Options{Index: st, Master: m, Algorithm: c.Keys.Algorithm, RetireGrace: c.Keys.RetireGrace, Log: log})
}

// newSigner returns the Signer of serve: the keys service over st with the master key of
// keys.masterKeyEnv, or nil when signing is off. Signing is off when keys.masterKeyEnv is
// empty or names a variable that is not set, and the index holds no signing key: every apt
// repo is unsigned then, and each gets a warning. A variable that is set but empty or
// malformed is an error, and so is a missing master key while the index holds signing keys:
// only that key decrypts them, and their repos' clients pinned them.
func newSigner(ctx context.Context, c *config.Config, lookupEnv func(string) (string, bool), st index.Store, log *slog.Logger) (format.Signer, error) {
	m, err := keys.LoadMaster(c.Keys.MasterKeyEnv, lookupEnv)
	if errors.Is(err, keys.ErrNoMasterKey) {
		return nil, signingOff(ctx, st, err, log)
	}
	if err != nil {
		return nil, err
	}
	svc, err := newKeys(c, st, m, log)
	if err != nil {
		return nil, err
	}
	log.Info("apt repos are signed with keys under the master key", "keys.masterKeyEnv", c.Keys.MasterKeyEnv,
		"keys.algorithm", c.Keys.Algorithm, "keys.retireGrace", c.Keys.RetireGrace.String())
	return svc, nil
}

// signingOff checks that serve may run without the master key, whose absence cause says: an
// error when the index holds signing keys; otherwise a warning for every apt repo, or one line
// when there is none, and nil.
func signingOff(ctx context.Context, st index.Store, cause error, log *slog.Logger) error {
	repos, err := st.ListRepos(ctx, "")
	if err != nil {
		return err
	}
	held := 0
	var apt []string
	for _, r := range repos {
		ks, err := st.ListKeys(ctx, &r.ID)
		if err != nil {
			return err
		}
		for _, k := range ks {
			if k.Kind == keys.KindGPG {
				held++
			}
		}
		if r.Format == aptFormat {
			apt = append(apt, aptFormat+"/"+r.Name)
		}
	}
	if held > 0 {
		return fmt.Errorf("%w, and the index holds %d signing keys that only the master key decrypts", cause, held)
	}
	for _, name := range apt {
		log.WarnContext(ctx, "apt repo is unsigned: no master key", "repo", name, "reason", cause.Error())
	}
	if len(apt) == 0 {
		log.InfoContext(ctx, "apt signing is off: no master key", "reason", cause.Error())
	}
	return nil
}
