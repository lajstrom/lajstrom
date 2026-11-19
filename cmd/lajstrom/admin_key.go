package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/gcpauth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/keys/awskms"
	"github.com/lajstrom/lajstrom/internal/keys/gcpkms"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// signedFormats are the formats whose repos have signing keys: apt (internal/format/deb), whose
// Release they sign, and rpm (internal/format/rpm), whose repomd.xml they sign.
var signedFormats = map[string]bool{"apt": true, "rpm": true}

// ephemeralTokens says what a server without a master key does with bearer tokens.
const ephemeralTokens = "bearer tokens are signed with an ephemeral key: they do not survive a restart and are not " +
	"shared between replicas; set keys.masterKeyEnv, keys.masterKeyFile or keys.masterKeyDir"

// newAdminKeyCmd returns admin key: export, rotate and drop the signing keys in the index
// database, without HTTP: the signing key of an apt repo (--repo) or the key that signs the
// bearer tokens of /v2 (--token, ADR 0010). Every command needs the master key, from the file
// of keys.masterKeyFile or the variable keys.masterKeyEnv names, and assumes that lajstrom
// migrate ran.
func newAdminKeyCmd(a *app) *cobra.Command {
	key := &cobra.Command{
		Use:   "key",
		Short: "Export, rotate and drop the signing keys of apt and rpm repos and of bearer tokens; rewrap under a new master key",
	}
	key.AddCommand(newKeyExportCmd(a), newKeyRotateCmd(a), newKeyDropCmd(a), newKeyRewrapCmd(a))
	return key
}

// keyFlags adds --repo and --token to cmd, one of them required and not both.
func keyFlags(cmd *cobra.Command, repo *string, tok *bool) {
	cmd.Flags().StringVar(repo, "repo", "", "the apt or rpm repo, apt/<name> or rpm/<name>")
	cmd.Flags().BoolVar(tok, "token", false, "the key that signs the bearer tokens of /v2")
	cmd.MarkFlagsMutuallyExclusive("repo", "token")
	cmd.MarkFlagsOneRequired("repo", "token")
}

// newKeyExportCmd returns admin key export: print the armored keyring of an apt repo, what
// apt's signed-by= points at, the same bytes as GET /apt/<name>/key.asc; or, with --token, the
// public key of the active bearer-token key as a PEM block, for a verifier that is not
// Lajstrom. A repo without a key, or a server without a token key, gets it now.
func newKeyExportCmd(a *app) *cobra.Command {
	var repo string
	var tok bool
	cmd := &cobra.Command{
		Use:         "export (--repo apt/<name> | --token)",
		Short:       "Print the armored keyring of an apt or rpm repo (signed-by=, gpgkey=), or the public bearer-token key as PEM",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if tok {
				return a.withTokenKeys(cmd.Context(), func(ctx context.Context, t *keys.Tokens) error {
					b, err := t.ExportToken(ctx)
					if err != nil {
						return err
					}
					_, err = cmd.OutOrStdout().Write(b)
					return err
				})
			}
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
	keyFlags(cmd, &repo, &tok)
	return cmd
}

// newKeyRotateCmd returns admin key rotate: give an apt repo a new signing key, retire the
// old one (it signs beside the new one and stays in key.asc for keys.retireGrace), drop the
// repo's cached indexes, and print the new fingerprint. The new key is the KMS key of --kms,
// a key Lajstrom makes with --local, and otherwise the KMS key of keys.kms or, without it, a
// key Lajstrom makes (ADR 0020). With --token: a new bearer-token key signs from now on, on
// every replica, and the old one verifies for the token lifetime plus the clock skew; with
// --drop as well, not at all, and every outstanding token dies.
func newKeyRotateCmd(a *app) *cobra.Command {
	var repo, kms string
	var tok, drop, local, encryption bool
	cmd := &cobra.Command{
		Use:         "rotate (--repo apt/<name> [--kms <key URI> | --local] | --repo <format>/<name> --encryption | --token [--drop])",
		Short:       "Give an apt or rpm repo, or the bearer tokens, a new signing key and print its fingerprint",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if drop && !tok {
				return errors.New("--drop goes with --token: an apt repo's retired key is dropped with keys.retireGrace: 0s")
			}
			if tok && (kms != "" || local) {
				return errors.New("--kms and --local go with --repo: the bearer-token key is always made by Lajstrom")
			}
			if encryption {
				return a.rotateEncryption(cmd, repo, tok || kms != "" || local)
			}
			if tok {
				return a.withTokenKeys(cmd.Context(), func(ctx context.Context, t *keys.Tokens) error {
					k, err := t.RotateToken(ctx, drop)
					if err != nil {
						return err
					}
					_, err = fmt.Fprintln(cmd.OutOrStdout(), k.ID())
					return err
				})
			}
			if err := checkKMSFlag(kms); err != nil {
				return err
			}
			return a.withKeys(cmd.Context(), repo, func(ctx context.Context, svc *keys.Service, r index.Repo) error {
				rotate := svc.Rotate
				switch {
				case kms != "":
					rotate = func(ctx context.Context, r index.Repo) (*keys.Entity, error) { return svc.RotateTo(ctx, r, kms) }
				case local:
					rotate = func(ctx context.Context, r index.Repo) (*keys.Entity, error) { return svc.RotateTo(ctx, r, keys.Local) }
				}
				k, err := rotate(ctx, r)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), k.Key.Fingerprint)
				return err
			})
		},
	}
	keyFlags(cmd, &repo, &tok)
	cmd.Flags().BoolVar(&drop, "drop", false, "with --token: the old key stops verifying at once, and every outstanding token dies")
	cmd.Flags().StringVar(&kms, "kms", "", "with --repo: the new key is this key of a KMS, awskms:///<key> or gcpkms://<key version> (ADR 0020)")
	cmd.Flags().BoolVar(&local, "local", false, "with --repo: the new key is made by Lajstrom, even with keys.kms set")
	cmd.Flags().BoolVar(&encryption, "encryption", false, "with --repo: rotate the key of an encrypted repo, made from its "+
		"policy.encryption.key, and wrap every data key again for it (ADR 0009)")
	cmd.MarkFlagsMutuallyExclusive("kms", "local")
	return cmd
}

// checkKMSFlag checks the key URI of --kms, when it is given.
func checkKMSFlag(kms string) error {
	if kms == "" {
		return nil
	}
	if _, err := config.ParseKMSKey(kms); err != nil {
		return fmt.Errorf("--kms: %w", err)
	}
	return nil
}

// rotateEncryption is admin key rotate --repo <repo> --encryption: a new key for the encrypted
// repo, the old one retired, then every data key of the repo wrapped again for the new key, one
// keys referrer per manifest (docs/spec/manifest-profile.md §11.1). It prints the new fingerprint
// and the number of manifests wrapped again. Run again after a rotation that stopped, it makes no
// other key and continues the wrapping.
func (a *app) rotateEncryption(cmd *cobra.Command, arg string, otherFlags bool) error {
	if otherFlags {
		return errors.New("--encryption goes with --repo alone: the new key is what the repo's policy.encryption.key names")
	}
	formatName, name, err := splitRepo(arg)
	if err != nil {
		return err
	}
	return a.withMaster(cmd.Context(), "", func(ctx context.Context, m *keys.Master, st store) error {
		repo, err := st.GetRepo(ctx, formatName, name)
		if errors.Is(err, index.ErrNotFound) {
			return fmt.Errorf("repo %s does not exist", arg)
		}
		if err != nil {
			return err
		}
		pol, err := format.EncryptionOf(*repo)
		if err != nil || !pol.AtRest() {
			return errors.Join(err, fmt.Errorf("repo %s does not encrypt at rest: its policy.encryption.mode is not at-rest", arg))
		}
		enc, err := keys.NewEncryption(keys.EncryptionOptions{Index: st, Master: m, KMS: decryptClients(a.lookupEnv, a.log), Log: a.log})
		if err != nil {
			return err
		}
		providers, err := provider.NewSet(a.cfg.Registries, a.log)
		if err != nil {
			return err
		}
		pipe := ingest.New(ingest.Options{Providers: providers, Index: st, Log: a.log, ClaimTTL: a.cfg.Ingest.ClaimTTL, Encryption: enc})
		active, err := unfinishedRotation(ctx, st, *repo)
		if err != nil {
			return err
		}
		if active == "" {
			row, _, err := enc.Rotate(ctx, *repo)
			if err != nil {
				return err
			}
			active = row.Fingerprint
		}
		n, err := pipe.Rewrap(ctx, *repo, auth.PrincipalFrom(ctx))
		if err != nil {
			return fmt.Errorf("%w; the key %s is active: run the rotation again to finish the wrapping", err, active)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n%d manifests wrapped again\n", active, n)
		return err
	})
}

// unfinishedRotation returns the active encryption key of repo when a rotation to it has not
// wrapped every data key yet (a row of blob_map names another key), "" otherwise.
func unfinishedRotation(ctx context.Context, st store, repo index.Repo) (string, error) {
	active, err := st.GetKey(ctx, &repo.ID, keys.KindEnc)
	if errors.Is(err, index.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	rows, err := st.ListBlobMap(ctx, repo.ID, "")
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.Kind == index.BlobMapBlob && r.KeyFingerprint != active.Fingerprint {
			return active.Fingerprint, nil
		}
	}
	return "", nil
}

// newKeyDropCmd returns admin key drop --token: retire the active bearer-token key without a
// successor, for an active key that does not decrypt (ADR 0010): serve refuses to start with
// one, and makes a new key at its next start once it is dropped, as a running replica does at
// its next token. It prints the fingerprint dropped. The key need not decrypt; the master key
// is needed all the same, as for the other commands.
func newKeyDropCmd(a *app) *cobra.Command {
	var tok bool
	cmd := &cobra.Command{
		Use:         "drop --token",
		Short:       "Retire the bearer-token key without a successor (the next token or start makes one) and print its fingerprint",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withTokenKeys(cmd.Context(), func(ctx context.Context, t *keys.Tokens) error {
				fp, err := t.DropToken(ctx)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), fp)
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&tok, "token", false, "the key that signs the bearer tokens of /v2 (required)")
	_ = cmd.MarkFlagRequired("token") // fails only for a flag that does not exist
	return cmd
}

// withKeys loads the master key, opens the index database, looks up the apt repo arg and runs
// fn with the keys service over the database, then closes it.
func (a *app) withKeys(ctx context.Context, arg string, fn func(context.Context, *keys.Service, index.Repo) error) error {
	formatName, name, err := splitRepo(arg)
	if err != nil {
		return err
	}
	if !signedFormats[formatName] {
		return fmt.Errorf("repo %q: only apt and rpm repos have signing keys", arg)
	}
	return a.withMaster(ctx, "", func(ctx context.Context, m *keys.Master, st store) error {
		repo, err := st.GetRepo(ctx, formatName, name)
		if errors.Is(err, index.ErrNotFound) {
			return fmt.Errorf("repo %s does not exist", arg)
		}
		if err != nil {
			return err
		}
		svc, err := newKeys(a.cfg, a.lookupEnv, st, m, a.log)
		if err != nil {
			return err
		}
		return fn(ctx, svc, *repo)
	})
}

// withTokenKeys is withKeys for --token: fn gets the bearer-token keys over the database.
// Without a master key there is no token key to act on, and the error says what serve does
// then.
func (a *app) withTokenKeys(ctx context.Context, fn func(context.Context, *keys.Tokens) error) error {
	return a.withMaster(ctx, "; without it "+ephemeralTokens, func(ctx context.Context, m *keys.Master, st store) error {
		t, err := newTokens(a.cfg, st, m, a.log)
		if err != nil {
			return err
		}
		return fn(ctx, t)
	})
}

// withMaster loads the master keys, opens the index database, has the KMS unwrap the data keys of
// keys.masterKeyKMS (unwrapAll: one that does not open stops the command), reads the key the index
// names to seal (sealingAtStart: a command that does not hold it exits 1 before it writes) and
// runs fn with both, then closes the database. hint follows the error of a missing master key.
func (a *app) withMaster(ctx context.Context, hint string, fn func(context.Context, *keys.Master, store) error) (err error) {
	m, err := loadMasters(a.cfg.Keys, a.lookupEnv, a.log)
	if errors.Is(err, keys.ErrNoMasterKey) {
		return fmt.Errorf("admin key needs the master key: %w%s", err, hint)
	}
	if err != nil {
		return fmt.Errorf("admin key needs the master key: %w", err)
	}
	st, err := a.openMigrated(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	if err := a.unwrapAll(ctx, st, m, false); err != nil {
		return err
	}
	if err := sealingAtStart(ctx, st, m, hostName()); err != nil {
		return err
	}
	return fn(ctx, m, st)
}

// newKeys returns the keys service of the configuration over st, with the KMS clients of
// kmsClients.
func newKeys(c *config.Config, lookupEnv func(string) (string, bool), st index.Store, m *keys.Master, log *slog.Logger) (*keys.Service, error) {
	return keys.New(keys.Options{Index: st, Master: m, Algorithm: c.Keys.Algorithm, RetireGrace: c.Keys.RetireGrace, Log: log,
		KMS: kmsClients(lookupEnv, log), KMSKey: c.Keys.KMS})
}

// kmsClients returns the KMS clients of the keys service by scheme (ADR 0020): AWS KMS at the
// endpoint of AWS_ENDPOINT_URL_KMS, else AWS_ENDPOINT_URL, else the region's, with the SDK's
// default chain of credentials; GCP Cloud KMS with the access token of the metadata server of
// GCE_METADATA_HOST, else 169.254.169.254. A client reads its credentials at its first request
// only, so a server with no key in a KMS never reads them.
func kmsClients(lookupEnv func(string) (string, bool), log *slog.Logger) map[string]keys.KMS {
	env := func(k string) string { v, _ := lookupEnv(k); return v }
	return map[string]keys.KMS{
		config.KMSSchemeAWS: awskms.New(awskms.Options{Endpoint: cmp.Or(env("AWS_ENDPOINT_URL_KMS"), env("AWS_ENDPOINT_URL")), Log: log}),
		config.KMSSchemeGCP: gcpkms.New(gcpkms.Options{MetadataHost: env(gcpauth.MetadataHostEnv), Log: log}),
	}
}

// newEncryption returns the keys of encrypted repos over st (ADR 0009) under the master keys m,
// with the KMS clients of decryptClients; nil without a master key, when no repo can be encrypted
// (api.Repos.Encryption refuses at-rest). signingOff has already refused a start without the
// master key whose index holds encryption keys.
func newEncryption(m *keys.Master, lookupEnv func(string) (string, bool), st index.Store, log *slog.Logger) (*keys.Encryption, error) {
	if m == nil {
		return nil, nil
	}
	return keys.NewEncryption(keys.EncryptionOptions{Index: st, Master: m, KMS: decryptClients(lookupEnv, log), Log: log})
}

// encryptionOf is newEncryption under the master keys of the configuration, with the key the
// index names to seal (holdMasters), for a command: nil without a master key.
func (a *app) encryptionOf(ctx context.Context, st store) (*keys.Encryption, error) {
	ms, err := a.holdMasters(ctx, st, a.log)
	if err != nil {
		return nil, err
	}
	return newEncryption(ms.m, a.lookupEnv, st, a.log)
}

// decryptClients returns the KMS clients that hold the decryption keys of encrypted repos, by
// scheme, configured as kmsClients configures them.
func decryptClients(lookupEnv func(string) (string, bool), log *slog.Logger) map[string]keys.DecryptKMS {
	out := map[string]keys.DecryptKMS{}
	for scheme, c := range kmsClients(lookupEnv, log) {
		if d, ok := c.(keys.DecryptKMS); ok {
			out[scheme] = d
		}
	}
	return out
}

// masterConfigured reports whether the configuration names master keys that load, for
// api.Repos.Encryption.
func masterConfigured(c *config.Config, lookupEnv func(string) (string, bool)) bool {
	_, err := loadMasters(c.Keys, lookupEnv, nil)
	return err == nil
}

// newTokens returns the bearer-token keys of the configuration over st.
func newTokens(c *config.Config, st index.Store, m *keys.Master, log *slog.Logger) (*keys.Tokens, error) {
	return keys.NewTokens(keys.TokenOptions{Index: st, Master: m, Algorithm: c.Keys.TokenAlgorithm, Log: log})
}

// newTokenKeys returns the bearer-token keys of serve, loaded or made now, before the server
// listens: the key of the index under the master keys of ms, made when there is none, with one
// line at info that names its fingerprint and algorithm. An active key that does not decrypt
// stops serve with an error that names it and admin key drop --token, but while a data key of
// keys.masterKeyKMS is not unwrapped yet (#1388): the key may be under it, and serve, which does
// not wait for the KMS, logs a warning and reads the key again at every token, 503 until then.
// Without a master key (none configured, or the variable not set) the keys are an ephemeral key
// in memory, with a warning that says why.
func newTokenKeys(ctx context.Context, c *config.Config, ms masters, st index.Store, log *slog.Logger) (token.Keys, error) {
	if ms.m == nil {
		e := keys.NewEphemeralTokens()
		k, _ := e.TokenSigner(ctx) // the ephemeral key: never an error
		log.WarnContext(ctx, ephemeralTokens, "fingerprint", k.ID(), "algorithm", k.Algorithm, "reason", ms.none.Error())
		return e, nil
	}
	t, err := newTokens(c, st, ms.m, log)
	if err != nil {
		return nil, err
	}
	k, err := t.TokenSigner(ctx)
	switch {
	case err != nil && len(ms.m.Pending()) > 0 && (errors.Is(err, keys.ErrKMSUnavailable) || errors.Is(err, keys.ErrNoSealingKey)):
		log.WarnContext(ctx, "the bearer-token key is not loaded yet: a master key is not unwrapped, and /v2/token answers 503 "+
			"until it is", "pending", ms.m.Pending(), "error", err.Error())
		return t, nil
	case err != nil:
		return nil, fmt.Errorf("the bearer-token key: %w", err)
	}
	log.InfoContext(ctx, "bearer tokens are signed with the key in the database",
		append([]any{"fingerprint", k.ID(), "algorithm", k.Algorithm}, masterSources(c.Keys)...)...)
	return t, nil
}

// newSigner returns the Signer of serve: the keys service over st with the master keys of ms, or
// nil when signing is off. Signing is off when no master key is configured, or the variable is not
// set, and the index holds no signing key: every apt repo is unsigned then, and each gets a
// warning. A missing master key while the index holds signing keys is an error: only that key
// decrypts them, and their repos' clients pinned them.
func newSigner(ctx context.Context, c *config.Config, ms masters, lookupEnv func(string) (string, bool), st index.Store,
	log *slog.Logger) (format.Signer, error) {
	if ms.m == nil {
		return nil, signingOff(ctx, st, ms.none, log)
	}
	svc, err := newKeys(c, lookupEnv, st, ms.m, log)
	if err != nil {
		return nil, err
	}
	log.Info("apt repos are signed with keys under the master key", append(masterSources(c.Keys),
		"keys.algorithm", c.Keys.Algorithm, "keys.retireGrace", c.Keys.RetireGrace.String(), "keys.kms", c.Keys.KMS)...)
	return svc, nil
}

// signingOff checks that serve may run without the master key, whose absence cause says: an
// error when the index holds apt signing keys; otherwise a warning for every apt repo, or one
// line when there is none, and nil. Bearer-token keys do not count: no client pinned them, and
// serve signs tokens with an ephemeral key then.
func signingOff(ctx context.Context, st index.Store, cause error, log *slog.Logger) error {
	repos, err := st.ListRepos(ctx, "")
	if err != nil {
		return err
	}
	held := 0
	var unsigned []index.Repo
	for _, r := range repos {
		ks, err := st.ListKeys(ctx, &r.ID)
		if err != nil {
			return err
		}
		for _, k := range ks {
			if k.Kind == keys.KindGPG || k.Kind == keys.KindEnc {
				held++
			}
		}
		if signedFormats[r.Format] {
			unsigned = append(unsigned, r)
		}
	}
	if held > 0 {
		return fmt.Errorf("%w, and the index holds %d signing or encryption keys that only the master key decrypts", cause, held)
	}
	for _, r := range unsigned {
		log.WarnContext(ctx, r.Format+" repo is unsigned: no master key", "repo", r.Format+"/"+r.Name, "reason", cause.Error())
	}
	if len(unsigned) == 0 {
		log.InfoContext(ctx, "apt signing is off: no master key", "reason", cause.Error())
	}
	return nil
}
