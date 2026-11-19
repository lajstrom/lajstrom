package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// actionKeyWrap is the audit action of a data key of the master key made or wrapped again, subject
// master:<id> (docs/spec/rbac.md §5, #1388).
const actionKeyWrap = "key.wrap"

// newKeyWrapCmd returns admin key wrap: make a data key of the master key, wrapped by a symmetric
// key of AWS KMS or GCP Cloud KMS, or wrap one of keys.masterKeyKMS again under its KMS key
// (docs/spec/index-schema.md, A master key in a KMS, #1388). It reads no master key.
func newKeyWrapCmd(a *app) *cobra.Command {
	var kmsURI, id string
	cmd := &cobra.Command{
		Use:   "wrap (--kms <key URI> | --id <id>)",
		Short: "Make a master key that a KMS key wraps and print its entry of keys.masterKeyKMS, or wrap one again",
		Long: "Make a data key of the master key: 32 random bytes that the symmetric KMS key of --kms wraps, AWS KMS\n" +
			"(awskms:///<key id | key ARN | alias>) or GCP Cloud KMS (gcpkms://projects/…/cryptoKeys/<k>, no version). It\n" +
			"checks that the KMS unwraps what it wrapped, writes the row of wrapped_master_keys with its audit row key.wrap,\n" +
			"and prints the id and the entry of keys.masterKeyKMS, never the key nor its ciphertext. Add the entry to the\n" +
			"configuration of every process, then make it the key that seals with lajstrom admin key rewrap --to <id>.\n" +
			"With --id it wraps a data key of keys.masterKeyKMS again under its KMS key, after a rotation of a GCP key.",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if id != "" && !masterKeyID.MatchString(id) {
				return usageError("--id %q: the id of a master key is 16 lowercase hexadecimal digits", id)
			}
			if id != "" {
				return a.wrapAgain(cmd.Context(), cmd.OutOrStdout(), id)
			}
			return a.wrapNew(cmd.Context(), cmd.OutOrStdout(), kmsURI)
		},
	}
	cmd.Flags().StringVar(&kmsURI, "kms", "", "the symmetric KMS key that wraps the new key: awskms:///<key id | key ARN | alias> "+
		"or gcpkms://projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>")
	cmd.Flags().StringVar(&id, "id", "", "wrap again, under its KMS key, the data key of keys.masterKeyKMS of this id")
	cmd.MarkFlagsMutuallyExclusive("kms", "id")
	cmd.MarkFlagsOneRequired("kms", "id")
	return cmd
}

// wrapKeyOf parses the key URI of --kms: an AWS key by its id, ARN, alias or alias ARN, which
// SymmetricKey resolves to its ARN, or a GCP key without a version.
func wrapKeyOf(uri string) (config.KMSKey, error) {
	if strings.HasPrefix(uri, config.KMSSchemeGCP+"://") {
		return config.ParseMasterKMSKey(uri)
	}
	return config.ParseKMSKey(uri)
}

// symmetricKey returns the client of key and key by the canonical name the KMS gives it, once the
// KMS has described it as a symmetric key for encryption, enabled (SymmetricKey).
func (a *app) symmetricKey(ctx context.Context, key config.KMSKey) (keys.MasterKMS, config.KMSKey, error) {
	c := a.masterKMS()[key.Scheme]
	if c == nil {
		return nil, config.KMSKey{}, fmt.Errorf("%s: this binary has no client for %s", key, key.Scheme)
	}
	name, err := c.SymmetricKey(ctx, key.Name)
	if err != nil {
		return nil, config.KMSKey{}, fmt.Errorf("the KMS key %s: %w", key, err)
	}
	canonical := config.KMSKey{Scheme: key.Scheme, Name: name}
	if _, err := config.ParseMasterKMSKey(canonical.String()); err != nil {
		return nil, config.KMSKey{}, fmt.Errorf("the KMS key %s: the KMS named it %w", key, err)
	}
	return c, canonical, nil
}

// wrapNew is admin key wrap --kms: the key described, the data key made and checked (keys.WrapMaster),
// then its row and key.wrap in one transaction, and its entry printed.
func (a *app) wrapNew(ctx context.Context, out io.Writer, uri string) (err error) {
	key, err := wrapKeyOf(uri)
	if err != nil {
		return fmt.Errorf("--kms: %w", err)
	}
	st, err := a.openMigrated(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	c, key, err := a.symmetricKey(ctx, key)
	if err != nil {
		return err
	}
	id, wrapped, err := keys.WrapMaster(ctx, c, key)
	if err != nil {
		return err
	}
	row := index.WrappedMasterKey{KeyID: id, KMS: key.String(), Wrapped: wrapped}
	if err := a.putWrapped(ctx, st, &row, nil); err != nil {
		return err
	}
	return printEntry(out, row)
}

// wrapAgain is admin key wrap --id: the data key of keys.masterKeyKMS of id, its row checked as at
// a start (dataKeysOf), wrapped again under its KMS key (keys.RewrapMaster), and the row replaced
// where it still holds the ciphertext read, with key.wrap, in one transaction.
func (a *app) wrapAgain(ctx context.Context, out io.Writer, id string) (err error) {
	i := slices.IndexFunc(a.cfg.Keys.MasterKeyKMS, func(e config.MasterKeyKMS) bool { return e.ID == id })
	if i < 0 {
		return fmt.Errorf("--id %s is not a data key of keys.masterKeyKMS: only a key that a KMS key wraps is wrapped again, "+
			"never a key of a file", id)
	}
	st, err := a.openMigrated(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	ds, err := dataKeysOf(ctx, st, a.cfg.Keys.MasterKeyKMS[i:i+1], a.masterKMS())
	if err != nil {
		return err
	}
	d := ds[0]
	if _, _, err := a.symmetricKey(ctx, d.key); err != nil {
		return err
	}
	wrapped, err := keys.RewrapMaster(ctx, d.kms, d.key, d.row)
	if err != nil {
		return err
	}
	row := index.WrappedMasterKey{KeyID: id, KMS: d.row.KMS, Wrapped: wrapped}
	if err := a.putWrapped(ctx, st, &row, d.row.Wrapped); err != nil {
		return err
	}
	return printEntry(out, row)
}

// putWrapped writes row, a new one where old is nil, else over the ciphertext old, with its audit
// row key.wrap, in one transaction: no row without its record.
func (a *app) putWrapped(ctx context.Context, st store, row *index.WrappedMasterKey, old []byte) error {
	ev := index.AuditEvent{Principal: auth.Anonymous.String(), Action: actionKeyWrap, Subject: "master:" + row.KeyID,
		Details: rewrapDetails(map[string]any{"kms": row.KMS, "new": old == nil}, a.cliOrigin())}
	err := st.Tx(ctx, func(w index.Writer) error {
		if err := w.AppendAudit(ctx, &ev); err != nil {
			return err
		}
		row.AuditID = &ev.ID
		return w.PutWrappedMasterKey(ctx, row, old)
	})
	if errors.Is(err, index.ErrConflict) {
		return fmt.Errorf("master key %s: the row of wrapped_master_keys changed in between, nothing was written: %w", row.KeyID, err)
	}
	return err
}

// printEntry prints the id of row, then its entry of keys.masterKeyKMS as YAML, never the key nor
// its ciphertext.
func printEntry(out io.Writer, row index.WrappedMasterKey) error {
	_, err := fmt.Fprintf(out, "%s\nkeys:\n  masterKeyKMS:\n    - {id: %s, kms: %q}\n", row.KeyID, row.KeyID, row.KMS)
	return err
}
