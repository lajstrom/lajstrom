package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/snapshot"
	"github.com/lajstrom/lajstrom/internal/version"
)

// actionSnapshotCreate is the audit action of a snapshot written into the registry: the subject
// is lajstrom/control-plane:<tag>, the digest the manifest's (docs/spec/index-schema.md,
// Snapshots of the control plane).
const actionSnapshotCreate = "snapshot.create"

// newAdminSnapshotCmd returns admin snapshot: write the control plane of the index into the
// registry as a snapshot, and list the snapshots there (#76; docs/operations.md, Snapshots of
// the control plane). Every command talks to the database and the registry itself, as reindex
// does, and assumes that lajstrom migrate ran.
func newAdminSnapshotCmd(a *app) *cobra.Command {
	snap := &cobra.Command{
		Use:   "snapshot",
		Short: "Snapshot the control plane into the registry and list the snapshots",
	}
	snap.AddCommand(newSnapshotCreateCmd(a), newSnapshotListCmd(a))
	return snap
}

// registryFlag adds --registry to cmd.
func registryFlag(cmd *cobra.Command, name *string) {
	cmd.Flags().StringVar(name, "registry", "", "the registry of the snapshots, a registries[].name; the one of the configuration "+
		"when it has one")
}

// createdSnapshot is what admin snapshot create prints: where the snapshot is, what its
// manifest says, and how many rows of each table it holds.
type createdSnapshot struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	snapshot.Info
	Rows map[string]int `json:"rows"`
}

// newSnapshotCreateCmd returns admin snapshot create: read the control plane in one transaction
// and write it into the registry, sealed with the master key unless --unsealed, with the audit
// rows of --audit; then append the audit row snapshot.create and print the snapshot as JSON.
func newSnapshotCreateCmd(a *app) *cobra.Command {
	var (
		registry, audit string
		unsealed        bool
	)
	cmd := &cobra.Command{
		Use:   "create [--registry <name>] [--unsealed] [--audit <age>|all]",
		Short: "Write the control plane into the registry as a snapshot, sealed with the master key, and print it as JSON",
		Long: "Write the control plane of the index (repos, group members, users, OIDC identities, tokens, bindings,\n" +
			"keys) into the registry as one manifest of lajstrom/control-plane below its prefix, under a tag of its\n" +
			"time. The rows are read in one transaction. The snapshot is sealed with a key derived from the master key;\n" +
			"--unsealed writes it readable by anyone who can read the registry, which holds password hashes and token\n" +
			"hashes, never a secret in clear. --audit 90d adds the audit rows of the last 90 days, --audit all every\n" +
			"row. A scheduler runs it periodically, as it runs admin retention.",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			ctx := cmd.Context()
			opts, err := auditOptions(audit, a.clock())
			if err != nil {
				return err
			}
			need := masterRequired
			if unsealed {
				need = masterNone
			}
			reg, name, err := a.snapshotRegistry(registry, need)
			if err != nil {
				return err
			}
			st, err := a.openMigrated(ctx)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, st.Close()) }()
			cp, err := st.ExportControlPlane(ctx, opts)
			if err != nil {
				return err
			}
			info, err := reg.Write(ctx, cp, !unsealed, version.Get().Version)
			if err != nil {
				return err
			}
			out := createdSnapshot{Registry: name, Repository: profile.SnapshotRepository, Info: info, Rows: rowCounts(cp)}
			if err := st.AppendAudit(ctx, snapshotEvent(actionSnapshotCreate, out)); err != nil {
				return fmt.Errorf("the snapshot %s:%s is written, its audit row is not: %w", out.Repository, info.Tag, err)
			}
			return writeJSONLine(cmd.OutOrStdout(), out)
		},
	}
	registryFlag(cmd, &registry)
	cmd.Flags().BoolVar(&unsealed, "unsealed", false, "write the snapshot without sealing it: no master key needed to write or read it")
	cmd.Flags().StringVar(&audit, "audit", "", "add the audit rows younger than this age (90d, 36h), or every row with all; none by default")
	return cmd
}

// newSnapshotListCmd returns admin snapshot list: the snapshots of the registry, the newest
// first, as a table or with --json one object per line.
func newSnapshotListCmd(a *app) *cobra.Command {
	var (
		registry string
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:         "list [--registry <name>] [--json]",
		Short:       "List the snapshots of the control plane in the registry, the newest first",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, _, err := a.snapshotRegistry(registry, masterNone)
			if err != nil {
				return err
			}
			infos, err := reg.List(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				for _, info := range infos {
					if err := writeJSONLine(cmd.OutOrStdout(), info); err != nil {
						return err
					}
				}
				return nil
			}
			return writeSnapshotTable(cmd.OutOrStdout(), infos)
		},
	}
	registryFlag(cmd, &registry)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print each snapshot as JSON, one per line")
	return cmd
}

// writeSnapshotTable prints the snapshots with the columns TAG, CREATED, SCHEMA, SEALED, SIZE and
// DIGEST.
func writeSnapshotTable(out io.Writer, infos []snapshot.Info) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "TAG\tCREATED\tSCHEMA\tSEALED\tSIZE\tDIGEST\n") // a bytes.Buffer does not fail
	for _, i := range infos {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%v\t%d\t%s\n", i.Tag, i.Created.Format(time.RFC3339), i.Schema, i.Sealed, i.Size, i.Digest)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := out.Write(buf.Bytes())
	return err
}

// auditOptions are the ExportOptions of --audit: none when empty, every row with all, the rows
// younger than an age otherwise (auth.ParseLifetime: 90d, 36h).
func auditOptions(s string, now time.Time) (index.ExportOptions, error) {
	switch s {
	case "":
		return index.ExportOptions{}, nil
	case "all":
		return index.ExportOptions{WithAudit: true}, nil
	}
	d, err := auth.ParseLifetime(s)
	if err != nil || d <= 0 {
		return index.ExportOptions{}, fmt.Errorf("--audit %q: an age such as 90d or 36h, or all", s)
	}
	return index.ExportOptions{WithAudit: true, AuditSince: now.Add(-d)}, nil
}

// masterNeed is what a snapshot command needs of the master key.
type masterNeed int

const (
	masterNone     masterNeed = iota // list, and create --unsealed: none
	masterRequired                   // create: a sealed snapshot is sealed with it
	masterIfSet                      // restore: a sealed snapshot opens with it, an unsealed one needs none
)

// snapshotRegistry returns the snapshots of the registry name, the one of the configuration when
// name is empty and it has one, and that name, with the master key as need asks: loaded when set,
// and an error when required and missing.
func (a *app) snapshotRegistry(name string, need masterNeed) (snapshot.Registry, string, error) {
	regs := a.cfg.Registries
	if name == "" {
		if len(regs) != 1 {
			names := make([]string, 0, len(regs))
			for _, r := range regs {
				names = append(names, r.Name)
			}
			return snapshot.Registry{}, "", fmt.Errorf("--registry names the registry of the snapshots: the configuration has %d (%s)",
				len(regs), strings.Join(names, ", "))
		}
		name = regs[0].Name
	}
	var reg snapshot.Registry
	for _, r := range regs {
		if r.Name != name {
			continue
		}
		p, err := provider.New(r, a.log)
		if err != nil {
			return snapshot.Registry{}, "", err
		}
		reg.Provider = p
	}
	if reg.Provider == nil {
		return snapshot.Registry{}, "", fmt.Errorf("--registry %q is not a registries[].name of the configuration", name)
	}
	if need == masterNone {
		return reg, name, nil
	}
	m, err := loadMaster(a.cfg.Keys, a.lookupEnv, a.log)
	switch {
	case errors.Is(err, keys.ErrNoMasterKey) && need == masterIfSet:
		return reg, name, nil
	case errors.Is(err, keys.ErrNoMasterKey):
		return snapshot.Registry{}, "", fmt.Errorf("a sealed snapshot needs the master key; --unsealed writes it without: %w", err)
	case err != nil:
		return snapshot.Registry{}, "", err
	}
	reg.Master = m
	return reg, name, nil
}

// rowCounts is how many rows of each table cp holds; audit only when it has its rows.
func rowCounts(cp *index.ControlPlane) map[string]int {
	n := map[string]int{"repos": len(cp.Repos), "groupMembers": len(cp.GroupMembers), "users": len(cp.Users),
		"identities": len(cp.Identities), "identityGroups": len(cp.IdentityGroups), "tokens": len(cp.Tokens),
		"bindings": len(cp.Bindings), "keys": len(cp.Keys)}
	if cp.WithAudit {
		n["audit"] = len(cp.Audit)
	}
	return n
}

// snapshotEvent is the audit row of action on the snapshot s, by the anonymous principal with the
// source cli and the host, as admin's changes are.
func snapshotEvent(action string, s createdSnapshot) *index.AuditEvent {
	o := cliOrigin()
	details, _ := json.Marshal(map[string]any{"source": o.Source, "host": o.Host, "registry": s.Registry, "sealed": s.Sealed,
		"schema": s.Schema, "rows": s.Rows}) // strings, a bool, ints: never an error
	subject := s.Repository + ":" + s.Tag
	if s.Tag == "" { // read by its digest
		subject = s.Repository + "@" + s.Digest.String()
	}
	return &index.AuditEvent{Principal: auth.Anonymous.String(), Action: action, Subject: subject, Digest: s.Digest, Details: details}
}
