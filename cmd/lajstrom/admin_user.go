package main

import (
	"bytes"
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

	"github.com/lajstrom/lajstrom/internal/auth"
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

// newAdminUserCmd returns admin user: create, list, disable, enable local users and set their
// passwords in the index database, without HTTP, through the users service (auth.Users), so
// that the validation and the audit events are those the users API (#47) will have. A change is
// made by the anonymous principal with the source cli. Like admin repo it assumes that
// lajstrom migrate ran.
func newAdminUserCmd(a *app) *cobra.Command {
	user := &cobra.Command{
		Use:   "user",
		Short: "Manage local users",
	}
	user.AddCommand(newUserCreateCmd(a), newUserListCmd(a), newUserDisableCmd(a), newUserEnableCmd(a), newUserSetPasswordCmd(a),
		newUserRenameCmd(a), newUserDeleteCmd(a))
	return user
}

// passwordSources is the help of the password flags.
const passwordSources = "the password comes from standard input with --password-stdin, else from a prompt on the terminal"

// newUserCreateCmd returns admin user create: create a local user, with --admin the admin
// binding too (the bootstrap of docs/spec/rbac.md §6), and print it as JSON on one line.
func newUserCreateCmd(a *app) *cobra.Command {
	var (
		name     string
		admin    bool
		useStdin bool
	)
	cmd := &cobra.Command{
		Use:         "create --name <name> [--admin] [--password-stdin]",
		Short:       "Create a local user and print it as JSON; " + passwordSources,
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				if err := auth.CheckName(name); err != nil {
					return err
				}
				if _, err := users.Get(ctx, name); err == nil {
					return fmt.Errorf("user %s already exists", name) // before the prompt
				}
				password, err := a.readPassword(cmd, useStdin)
				if err != nil {
					return err
				}
				u, err := users.Create(ctx, name, password, admin, auth.Origin(cliOrigin()))
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
	_ = cmd.MarkFlagRequired("name") // fails only for a flag that does not exist
	return cmd
}

// newUserListCmd returns admin user list: the users as a table.
func newUserListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List the users",
		Args:        cobra.NoArgs,
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				list, err := users.List(ctx)
				if err != nil {
					return err
				}
				return writeUserTable(cmd.OutOrStdout(), list)
			})
		},
	}
}

// newUserDisableCmd returns admin user disable: the user's credentials are refused from the
// next request on.
func newUserDisableCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "disable <name>",
		Short:       "Disable a user: its credentials are refused from the next request on",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				return users.Disable(ctx, args[0], auth.Origin(cliOrigin()))
			})
		},
	}
}

// newUserRenameCmd returns admin user rename: the uid, and so the bindings and tokens, stay.
func newUserRenameCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "rename <name> <new name>",
		Short:       "Rename a user; its uid, bindings and tokens stay",
		Args:        cobra.ExactArgs(2),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				return users.Rename(ctx, args[0], args[1], auth.Origin(cliOrigin()))
			})
		},
	}
}

// newUserDeleteCmd returns admin user delete (docs/spec/rbac.md §1): the user's tokens are
// revoked and its bindings deleted, and its name is free for a new user, who inherits nothing.
func newUserDeleteCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "delete <name>",
		Short:       "Delete a user: revoke its tokens, delete its bindings, free its name; not undone",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				return users.Delete(ctx, args[0], auth.Origin(cliOrigin()))
			})
		},
	}
}

// newUserEnableCmd returns admin user enable.
func newUserEnableCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "enable <name>",
		Short:       "Enable a disabled user",
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				return users.Enable(ctx, args[0], auth.Origin(cliOrigin()))
			})
		},
	}
}

// newUserSetPasswordCmd returns admin user set-password: the old password stops working at the
// next request.
func newUserSetPasswordCmd(a *app) *cobra.Command {
	var useStdin bool
	cmd := &cobra.Command{
		Use:         "set-password <name> [--password-stdin]",
		Short:       "Set a user's password; " + passwordSources,
		Args:        cobra.ExactArgs(1),
		Annotations: needsConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withUsers(cmd.Context(), func(ctx context.Context, users *auth.Users) error {
				if _, err := users.Get(ctx, args[0]); err != nil {
					return err
				}
				password, err := a.readPassword(cmd, useStdin)
				if err != nil {
					return err
				}
				return users.SetPassword(ctx, args[0], password, auth.Origin(cliOrigin()))
			})
		},
	}
	cmd.Flags().BoolVar(&useStdin, "password-stdin", false, "read the password from standard input, without the trailing newline")
	return cmd
}

// withUsers opens the index database of the configuration, runs fn with the users service over
// it, and closes the database.
func (a *app) withUsers(ctx context.Context, fn func(context.Context, *auth.Users) error) (err error) {
	st, err := openStore(ctx, a.cfg.Database)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	return fn(ctx, &auth.Users{Index: st, Log: a.log})
}

// errNoPassword is the error when no source gives a password.
var errNoPassword = errors.New("no password: pass --password-stdin with the password on standard input, or run on a terminal to be prompted")

// errRemovedEnv is the error of a command that finds removedEnv set.
var errRemovedEnv = errors.New(removedEnv + " is not read any more: pass the password on standard input with --password-stdin, " +
	"or at the prompt, and unset the variable (docs/auth.md, Bootstrap)")

// readPassword returns the password of admin user create, set-password and admin bootstrap:
// with --password-stdin, the whole of standard input without the trailing newline; else a
// prompt without echo, twice, when standard input is a terminal; else errNoPassword. With
// removedEnv set it is errRemovedEnv, whatever the variable holds. The password is checked by
// the users service.
func (a *app) readPassword(cmd *cobra.Command, useStdin bool) (string, error) {
	if _, ok := a.lookupEnv(removedEnv); ok {
		return "", errRemovedEnv
	}
	in := cmd.InOrStdin()
	if useStdin {
		b, err := io.ReadAll(io.LimitReader(in, auth.MaxPasswordLen+3)) // the bound, a CR LF, and one byte to be too long
		if err != nil {
			return "", fmt.Errorf("read the password from standard input: %w", err)
		}
		s := strings.TrimSuffix(string(b), "\n")
		return strings.TrimSuffix(s, "\r"), nil
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

// writeUserTable prints users as a table with the columns NAME, UID, KIND, DISABLED and CREATED.
func writeUserTable(out io.Writer, users []auth.User) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "NAME\tUID\tKIND\tDISABLED\tCREATED\n") // a bytes.Buffer does not fail
	for _, u := range users {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", u.Name, u.UID, u.Kind, u.Disabled, u.CreatedAt.Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := out.Write(buf.Bytes())
	return err
}
