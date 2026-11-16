package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
)

// removedEnv is the variable admin user create and set-password read the password from before
// #440. A password in the environment outlives its first change and then looks real while it is
// not, so it is gone; a command that finds it set fails and says what to do instead. Its name
// says what it is for, not what it holds: gosec (G101) takes a constant named …password… for a
// credential.
const removedEnv = "LAJSTROM_ADMIN_PASSWORD"

// terminal is how a password is prompted for; the tests replace it.
var terminal = struct {
	isTerminal   func(fd int) bool
	readPassword func(fd int) ([]byte, error)
}{term.IsTerminal, term.ReadPassword}

// newAdminUserCmd returns admin user: create, list, disable, enable local users and service
// accounts, set the passwords of local users and the owners of service accounts in the index
// database, without HTTP, through the users service (auth.Users), so that the validation and the
// audit events are those of the users API (#47). A change is made by the anonymous principal
// with the source cli. Like admin repo it assumes that lajstrom migrate ran.
func newAdminUserCmd(a *app) *cobra.Command {
	user := &cobra.Command{
		Use:   "user",
		Short: "Manage local users and service accounts",
	}
	user.AddCommand(newUserCreateCmd(a), newUserListCmd(a), newUserDisableCmd(a), newUserEnableCmd(a), newUserSetPasswordCmd(a),
		newUserRenameCmd(a), newUserDeleteCmd(a), newUserSetOwnerCmd(a))
	return user
}

// passwordSources is the help of the password flags.
const passwordSources = "the password comes from standard input with --password-stdin, else from a prompt on the terminal"

// newUserCreateCmd returns admin user create: create a local user, with --admin the admin
// binding too (the bootstrap of docs/spec/rbac.md §6), or with --service and --owner a service
// account, which has no password (docs/spec/rbac.md §1), and print it as JSON on one line.
func newUserCreateCmd(a *app) *cobra.Command {
	var (
		name, owner     string
		admin, useStdin bool
		service         bool
	)
	cmd := &cobra.Command{
		Use: "create --name <name> [--admin] [--password-stdin] | --name <name> --service --owner user:<name>|group:<name>",
		Short: "Create a local user, or a service account with --service, and print it as JSON; " + passwordSources +
			"; a service account has none",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case service && owner == "":
				return errors.New("--service needs --owner: user:<name> of a person, or group:<name>")
			case !service && owner != "":
				return errors.New("--owner is a service account's: add --service")
			}
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				if service {
					u, err := users.CreateService(ctx, name, owner, a.cliOrigin())
					if err != nil {
						return err
					}
					return writeJSONLine(cmd.OutOrStdout(), u)
				}
				if err := auth.CheckName(name); err != nil {
					return err
				}
				if _, err := users.Get(ctx, name); err == nil {
					return fmt.Errorf("user %s already exists", name) // before the prompt
				}
				password, err := a.readPassword(cmd, useStdin, "")
				if err != nil {
					return err
				}
				u, err := users.Create(ctx, name, password, admin, a.cliOrigin())
				if err != nil {
					return err
				}
				return writeJSONLine(cmd.OutOrStdout(), u)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "the user name: lowercase letters, digits, '.', '_' and '-', at most 64 bytes")
	f.BoolVar(&admin, "admin", false, "also bind the admin role on */*: everything on every repo, the system and the audit log")
	f.BoolVar(&useStdin, "password-stdin", false, "read the password from standard input, without the trailing newline")
	f.BoolVar(&service, "service", false, "create a service account: a user for a system, without a password, which authenticates "+
		"with the tokens of admin token create --user")
	f.StringVar(&owner, "owner", "", "the owner of the service account: user:<name> of a person, or group:<name> of the OIDC logins")
	cmd.MarkFlagsMutuallyExclusive("service", "admin")
	cmd.MarkFlagsMutuallyExclusive("service", "password-stdin")
	_ = cmd.MarkFlagRequired("name") // fails only for a flag that does not exist
	return cmd
}

// newUserListCmd returns admin user list: the users, or those of --kind, as a table.
func newUserListCmd(a *app) *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:         "list [--kind local|oidc|service]",
		Short:       "List the users, or the users of a kind: --kind service lists the service accounts",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				list, err := users.List(ctx, kind)
				if err != nil {
					return err
				}
				return writeUserTable(cmd.OutOrStdout(), list)
			})
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "only the users of this kind: local, oidc or service")
	return cmd
}

// newUserSetOwnerCmd returns admin user set-owner: a service account gets another owner
// (docs/spec/rbac.md §1).
func newUserSetOwnerCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "set-owner <name> user:<name>|group:<name>",
		Short:       "Give a service account another owner: a person, user:<name>, or a group, group:<name>",
		Args:        cobra.ExactArgs(2),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				return users.SetOwner(ctx, args[0], args[1], a.cliOrigin())
			})
		},
	}
}

// newUserDisableCmd returns admin user disable: the user's credentials are refused from the
// next request on.
func newUserDisableCmd(a *app) *cobra.Command {
	var etag string
	cmd := &cobra.Command{
		Use:         "disable <name> [--if-match <etag>]",
		Short:       "Disable a user: its credentials are refused from the next request on",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				ctx, err := ifMatch(ctx, etag, api.TableUsers, "user "+args[0])
				if err != nil {
					return err
				}
				return ifMatched(users.Disable(ctx, args[0], a.cliOrigin()))
			})
		},
	}
	ifMatchFlag(cmd, &etag)
	return cmd
}

// newUserRenameCmd returns admin user rename: the uid, and so the bindings and tokens, stay.
func newUserRenameCmd(a *app) *cobra.Command {
	var etag string
	cmd := &cobra.Command{
		Use:         "rename <name> <new name> [--if-match <etag>]",
		Short:       "Rename a user; its uid, bindings and tokens stay",
		Args:        cobra.ExactArgs(2),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				ctx, err := ifMatch(ctx, etag, api.TableUsers, "user "+args[0])
				if err != nil {
					return err
				}
				return ifMatched(users.Rename(ctx, args[0], args[1], a.cliOrigin()))
			})
		},
	}
	ifMatchFlag(cmd, &etag)
	return cmd
}

// newUserDeleteCmd returns admin user delete (docs/spec/rbac.md §1): the user's tokens are
// revoked and its bindings deleted, and its name is free for a new user, who inherits nothing.
func newUserDeleteCmd(a *app) *cobra.Command {
	var etag string
	cmd := &cobra.Command{
		Use:         "delete <name> [--if-match <etag>]",
		Short:       "Delete a user: revoke its tokens, delete its bindings, free its name; not undone",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				ctx, err := ifMatch(ctx, etag, api.TableUsers, "user "+args[0])
				if err != nil {
					return err
				}
				return ifMatched(users.Delete(ctx, args[0], a.cliOrigin()))
			})
		},
	}
	ifMatchFlag(cmd, &etag)
	return cmd
}

// newUserEnableCmd returns admin user enable.
func newUserEnableCmd(a *app) *cobra.Command {
	var etag string
	cmd := &cobra.Command{
		Use:         "enable <name> [--if-match <etag>]",
		Short:       "Enable a disabled user",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				ctx, err := ifMatch(ctx, etag, api.TableUsers, "user "+args[0])
				if err != nil {
					return err
				}
				return ifMatched(users.Enable(ctx, args[0], a.cliOrigin()))
			})
		},
	}
	ifMatchFlag(cmd, &etag)
	return cmd
}

// newUserSetPasswordCmd returns admin user set-password: the old password stops working at the
// next request.
func newUserSetPasswordCmd(a *app) *cobra.Command {
	var (
		useStdin     bool
		passwordFile string
	)
	cmd := &cobra.Command{
		Use:         "set-password <name> [--password-stdin | --password-file <file>]",
		Short:       "Set a user's password; " + passwordSources + ", or from a file with --password-file",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				u, err := users.Get(ctx, args[0])
				if err != nil {
					return err
				}
				if u.Kind == auth.KindService { // before the prompt
					return fmt.Errorf("user %s is a service account, which has no password: it authenticates with the tokens "+
						"of admin token create --user %s", u.Name, u.Name)
				}
				password, err := a.readPassword(cmd, useStdin, passwordFile)
				if err != nil {
					return err
				}
				return users.SetPassword(ctx, args[0], password, a.cliOrigin())
			})
		},
	}
	cmd.Flags().BoolVar(&useStdin, "password-stdin", false, "read the password from standard input, without the trailing newline")
	// The recovery Job of the Helm chart: its image has no shell to put a Secret on standard input.
	cmd.Flags().StringVar(&passwordFile, "password-file", "", "read the password from this file, without the trailing newline")
	cmd.MarkFlagsMutuallyExclusive("password-stdin", "password-file")
	return cmd
}

// withUsers opens the index database of the configuration, runs fn with the users service over
// it, which disables the service accounts of an owner it disables when
// auth.serviceAccounts.onOwnerDisabled is disable and marks the users inactive beyond
// auth.disableInactiveUsersAfter, and closes the database.
func (a *app) withUsers(ctx context.Context, fn func(context.Context, *auth.Users) error) (err error) {
	st, err := a.openMigrated(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	return fn(ctx, &auth.Users{Index: st, Log: a.log, DisableWithOwner: a.cfg.Auth.ServiceAccounts.OnOwnerDisabled == config.OwnerDisable,
		InactiveAfter: a.cfg.Auth.DisableInactiveUsersAfter})
}

// errNoPassword is the error when no source gives a password.
var errNoPassword = errors.New("no password: pass --password-stdin with the password on standard input, or run on a terminal to be prompted")

// errRemovedEnv is the error of a command that finds removedEnv set.
var errRemovedEnv = errors.New(removedEnv + " is not read any more: pass the password on standard input with --password-stdin, " +
	"or at the prompt, and unset the variable (docs/auth.md, Bootstrap)")

// readPassword returns the password of admin user create, set-password and admin bootstrap:
// with file (set-password's --password-file), the whole of the file without the trailing
// newline; with --password-stdin, the whole of standard input without it; else a prompt without
// echo, twice, when standard input is a terminal; else errNoPassword. With removedEnv set it is
// errRemovedEnv, whatever the variable holds. The password is checked by the users service.
func (a *app) readPassword(cmd *cobra.Command, useStdin bool, file string) (string, error) {
	if _, ok := a.lookupEnv(removedEnv); ok {
		return "", errRemovedEnv
	}
	in := cmd.InOrStdin()
	switch {
	case file != "":
		f, err := os.Open(file) // #nosec G304 -- the operator names the file
		if err != nil {
			return "", fmt.Errorf("--password-file: %w", err)
		}
		defer func() { _ = f.Close() }() // read only
		return readPasswordFrom(f, file)
	case useStdin:
		return readPasswordFrom(in, "standard input")
	}
	f, ok := in.(*os.File)
	if !ok || !terminal.isTerminal(int(f.Fd())) {
		return "", errNoPassword
	}
	fd, prompt := int(f.Fd()), cmd.ErrOrStderr()
	first, err := promptPassword(prompt, fd, "Password: ")
	if err != nil {
		return "", err
	}
	again, err := promptPassword(prompt, fd, "Repeat the password: ")
	if err != nil {
		return "", err
	}
	if first != again {
		return "", errors.New("the passwords do not match")
	}
	return first, nil
}

// readPasswordFrom returns the whole of r, named from in its error, without the trailing
// newline (LF or CR LF): a password of --password-stdin or --password-file.
func readPasswordFrom(r io.Reader, from string) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, auth.MaxPasswordLen+3)) // the bound, a CR LF, and one byte to be too long
	if err != nil {
		return "", fmt.Errorf("read the password from %s: %w", from, err)
	}
	s := strings.TrimSuffix(string(b), "\n")
	return strings.TrimSuffix(s, "\r"), nil
}

// promptPassword writes label to out and reads a line from the terminal fd without echo.
func promptPassword(out io.Writer, fd int, label string) (string, error) {
	_, _ = io.WriteString(out, label)
	b, err := terminal.readPassword(fd)
	_, _ = io.WriteString(out, "\n")
	if err != nil {
		return "", fmt.Errorf("read the password from the terminal: %w", err)
	}
	return string(b), nil
}

// writeUserTable prints users as a table with the columns NAME, UID, KIND, DISABLED, CREATED, LAST
// SEEN and OWNER. LAST SEEN is the user's last activity, - before any record, with (inactive) for
// a user inactive beyond auth.disableInactiveUsersAfter (docs/spec/rbac.md §1, Inactive users).
// OWNER is a service account's, user:<name> of a person, with (disabled) while that user is, or
// group:<name>; - for every other kind.
func writeUserTable(out io.Writer, users []auth.User) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "NAME\tUID\tKIND\tDISABLED\tCREATED\tLAST SEEN\tOWNER\n") // a bytes.Buffer does not fail
	for _, u := range users {
		owner := cmp.Or(u.Owner, "-")
		if u.OwnerName != "" {
			owner = "user:" + u.OwnerName
		}
		if u.OwnerDisabled {
			owner += " (disabled)"
		}
		seen := "-"
		if u.LastSeenAt != nil {
			seen = u.LastSeenAt.Format(time.RFC3339)
		}
		if u.Inactive {
			seen += " (inactive)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\t%s\t%s\n", u.Name, u.UID, u.Kind, u.Disabled, u.CreatedAt.Format(time.RFC3339),
			seen, owner)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := out.Write(buf.Bytes())
	return err
}
