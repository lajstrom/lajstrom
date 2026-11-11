package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// runIn executes the command tree with args, the environment env and standard input in, and
// returns stdout, stderr and the error.
func runIn(t *testing.T, env map[string]string, in io.Reader, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newRootCmd(func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	cmd.SetIn(in)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

// fakeTerminal makes standard input a terminal whose lines are answers, in order, for the
// test, and then io.EOF; readErr, when set, is the error of every read.
func fakeTerminal(t *testing.T, readErr error, answers ...string) {
	t.Helper()
	saved := terminal
	t.Cleanup(func() { terminal = saved })
	terminal.isTerminal = func(int) bool { return true }
	terminal.readPassword = func(int) ([]byte, error) {
		if readErr != nil {
			return nil, readErr
		}
		if len(answers) == 0 {
			return nil, io.EOF
		}
		a := answers[0]
		answers = answers[1:]
		return []byte(a), nil
	}
}

// ttyFile returns a file to stand for the terminal of fakeTerminal: the prompt needs an
// *os.File.
func ttyFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// openTestStore opens the SQLite database at dsn for the checks of a test.
func openTestStore(t *testing.T, dsn string) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// wantPassword checks that the stored hash of user verifies password.
func wantPassword(t *testing.T, dsn, user, password string) {
	t.Helper()
	u, err := openTestStore(t, dsn).GetUser(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := auth.VerifyPassword(u.PasswordHash, password); !ok || err != nil {
		t.Errorf("the password of %s is not the one given: %v, %v", user, ok, err)
	}
}

// TestAdminUser runs the bootstrap and the life of a user: create with --admin, a conflict,
// create from standard input, list, disable, enable, set-password; the binding, the hashes and
// the audit events in the database.
func TestAdminUser(t *testing.T) {
	cfg, dsn := writeRepoConfig(t, t.TempDir())
	stdin := []string{"--password-stdin"}
	steps := []struct {
		env     map[string]string
		stdin   string
		args    []string
		wantErr string
		wantOut string
	}{
		{args: []string{"migrate"}, wantOut: "applied 0001"},
		{stdin: "admin-password", args: append([]string{"admin", "user", "create", "--name", "admin", "--admin"}, stdin...),
			wantOut: `","name":"admin","kind":"local","disabled":false,"createdAt":"`},
		{args: []string{"admin", "user", "create", "--name", "admin"}, wantErr: "user admin already exists"},
		{stdin: "alice-password\n", args: []string{"admin", "user", "create", "--name", "alice", "--password-stdin"},
			wantOut: `","name":"alice","kind":"local","disabled":false,`},
		{args: []string{"admin", "user", "disable", "alice"}},
		{args: []string{"admin", "user", "list"}, wantOut: "NAME   UID                                   KIND   DISABLED  CREATED\nadmin  0"},
		{args: []string{"admin", "user", "list"}, wantOut: "  local  true      20"},
		{args: []string{"admin", "user", "enable", "alice"}},
		{args: []string{"admin", "user", "list"}, wantOut: "\nalice  0"},
		{args: []string{"admin", "user", "rename", "alice", "alicia"}},
		{args: []string{"admin", "user", "list"}, wantOut: "\nalicia  0"},
		{args: []string{"admin", "user", "rename", "alicia", "admin"}, wantErr: "user admin already exists"},
		{args: []string{"admin", "user", "delete", "admin"}, wantErr: "the last enabled user with an admin binding"},
		{stdin: "new-alice-password\n", args: append([]string{"admin", "user", "set-password", "alicia"}, stdin...)},
		{args: []string{"admin", "user", "rename", "alicia", "alice"}},
		{stdin: "new-alice-password", args: append([]string{"admin", "user", "set-password", "alice"}, stdin...)},
		{args: []string{"admin", "user", "disable", "nobody"}, wantErr: "user nobody does not exist"},
		{args: []string{"admin", "user", "enable", "nobody"}, wantErr: "user nobody does not exist"},
		{args: []string{"admin", "user", "set-password", "nobody"}, wantErr: "user nobody does not exist"},
		{args: []string{"admin", "user", "create", "--name", "Bob"}, wantErr: `name: "Bob" must be lowercase`},
		{stdin: "short", args: append([]string{"admin", "user", "create", "--name", "bob"}, stdin...),
			wantErr: "password: must be at least 8 bytes"},
		{args: []string{"admin", "user", "create"}, wantErr: `required flag(s) "name" not set`},
		{env: map[string]string{removedEnv: "admin-password"}, stdin: "bob-password", args: append([]string{"admin", "user", "create", "--name", "bob"}, stdin...),
			wantErr: "LAJSTROM_ADMIN_PASSWORD is not read any more: pass the password on standard input with --password-stdin"},
		{args: []string{"admin", "user", "disable"}, wantErr: "accepts 1 arg(s)"},
	}
	for _, s := range steps {
		out, _, err := runIn(t, s.env, strings.NewReader(s.stdin), append(s.args, "--config", cfg)...)
		switch {
		case s.wantErr == "" && err != nil:
			t.Fatalf("%v: %v", s.args, err)
		case s.wantErr != "" && (err == nil || !strings.Contains(err.Error(), s.wantErr)):
			t.Fatalf("%v: error = %v, want it to contain %q", s.args, err, s.wantErr)
		}
		if !strings.Contains(out, s.wantOut) {
			t.Errorf("%v: stdout = %q, want it to contain %q", s.args, out, s.wantOut)
		}
		if strings.Contains(out, "argon2id") {
			t.Errorf("%v: stdout has a hash: %q", s.args, out)
		}
	}

	wantPassword(t, dsn, "admin", "admin-password")
	wantPassword(t, dsn, "alice", "new-alice-password")
	st := openTestStore(t, dsn)
	bindings, err := st.ListBindings(context.Background(), "")
	admin, _ := st.GetUser(context.Background(), "admin")
	if err != nil || admin == nil || len(bindings) != 1 || bindings[0].Principal != "user:"+admin.UID || bindings[0].Role != "admin" || bindings[0].RepoMask != "*/*" {
		t.Errorf("bindings = %+v, %v; want the admin binding of admin only", bindings, err)
	}
	wantCLIAudit(t, dsn, auth.ActionUserCreate, 2)
	wantCLIAudit(t, dsn, auth.ActionUserDisable, 1)
	wantCLIAudit(t, dsn, auth.ActionUserEnable, 1)
	wantCLIAudit(t, dsn, auth.ActionUserPassword, 2)
	wantCLIAudit(t, dsn, auth.ActionUserRename, 2)
}

// userCLI is a migrated SQLite database and its configuration, for admin user.
type userCLI struct{ cfg, dsn string }

func newUserCLI(t *testing.T) userCLI {
	t.Helper()
	cfg, dsn := writeRepoConfig(t, t.TempDir())
	if _, _, err := run(t, nil, "migrate", "--config", cfg); err != nil {
		t.Fatal(err)
	}
	return userCLI{cfg: cfg, dsn: dsn}
}

// create runs admin user create --name name with flags and returns stderr and the error.
func (c userCLI) create(t *testing.T, env map[string]string, in io.Reader, name string, flags ...string) (string, error) {
	t.Helper()
	_, stderr, err := runIn(t, env, in, append([]string{"admin", "user", "create", "--name", name, "--config", c.cfg}, flags...)...)
	return stderr, err
}

// TestAdminUserPasswordStdin checks --password-stdin: the whole of standard input without the
// trailing newline.
func TestAdminUserPasswordStdin(t *testing.T) {
	c := newUserCLI(t)
	tests := []struct {
		name    string
		env     map[string]string
		in      io.Reader
		stored  string // the password stored; "" when the create fails
		wantErr string
	}{
		{name: "with a newline", in: strings.NewReader("stdin-password\n"), stored: "stdin-password"},
		{name: "without CR LF", in: strings.NewReader("pass word with spaces\r\n"), stored: "pass word with spaces"},
		{name: "without a newline", in: strings.NewReader("no-newline-at-all"), stored: "no-newline-at-all"},
		{name: "inner newlines kept", in: strings.NewReader("two\nlines!\n"), stored: "two\nlines!"},
		{name: "too long", in: strings.NewReader(strings.Repeat("x", auth.MaxPasswordLen+1) + "\n"), wantErr: "at most 1024 bytes"},
		{name: "empty", in: strings.NewReader("\n"), wantErr: "at least 8 bytes"},
		{name: "read fails", in: iotestErrReader{}, wantErr: "read the password from standard input: broken pipe"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := fmt.Sprintf("u%d", i)
			_, err := c.create(t, tt.env, tt.in, name, "--password-stdin")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantPassword(t, c.dsn, name, tt.stored)
		})
	}
}

// TestAdminUserPrompt checks the prompt on a terminal: twice, without echo, on stderr, and the
// errors of a mismatch and of the terminal.
func TestAdminUserPrompt(t *testing.T) {
	c := newUserCLI(t)
	tests := []struct {
		name    string
		answers []string
		readErr error
		wantErr string // "" when the user is created with answers[0]
	}{
		{name: "twice the same", answers: []string{"prompted-password", "prompted-password"}},
		{name: "mismatch", answers: []string{"prompted-password", "another-password"}, wantErr: "the passwords do not match"},
		{name: "first read fails", readErr: errors.New("inappropriate ioctl"), wantErr: "read the password from the terminal: inappropriate ioctl"},
		{name: "second read fails", answers: []string{"prompted-password"}, wantErr: "read the password from the terminal: EOF"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeTerminal(t, tt.readErr, tt.answers...)
			name := fmt.Sprintf("p%d", i)
			stderr, err := c.create(t, nil, ttyFile(t), name)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if stderr != "Password: \nRepeat the password: \n" {
				t.Errorf("prompts = %q", stderr)
			}
			wantPassword(t, c.dsn, name, tt.answers[0])
		})
	}

	fakeTerminal(t, nil, "second-password", "second-password")
	if _, _, err := runIn(t, nil, ttyFile(t), "admin", "user", "set-password", "p0", "--config", c.cfg); err != nil {
		t.Fatal(err)
	}
	wantPassword(t, c.dsn, "p0", "second-password")
}

// TestAdminUserNoPassword checks the error when no source gives a password, which names the
// two ways, and that an invalid or taken name is refused before any prompt.
func TestAdminUserNoPassword(t *testing.T) {
	c := newUserCLI(t)
	for _, in := range []io.Reader{strings.NewReader("ignored-password\n"), ttyFile(t)} {
		_, err := c.create(t, nil, in, "nobody")
		if err == nil || err.Error() != errNoPassword.Error() {
			t.Errorf("error = %v, want %v", err, errNoPassword)
		}
	}
	for _, way := range []string{"--password-stdin", "terminal"} {
		if !strings.Contains(errNoPassword.Error(), way) {
			t.Errorf("%q does not name %s", errNoPassword, way)
		}
	}

	if _, err := c.create(t, nil, strings.NewReader("taken-password"), "taken", "--password-stdin"); err != nil {
		t.Fatal(err)
	}
	fakeTerminal(t, errors.New("prompted"))
	for name, want := range map[string]string{"Bad": `name: "Bad"`, "taken": "user taken already exists"} {
		if _, err := c.create(t, nil, ttyFile(t), name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("create %s: %v, want %q", name, err, want)
		}
	}
	users, err := openTestStore(t, c.dsn).ListUsers(context.Background())
	if err != nil || len(users) != 1 {
		t.Errorf("users = %+v, %v; want taken only", users, err)
	}
}

// iotestErrReader is a reader that fails.
type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }

// admin user does not migrate: a database without the schema is the store's error.
func TestAdminUserWithoutMigrate(t *testing.T) {
	cfg := writeConfig(t, t.TempDir())
	if _, _, err := run(t, nil, "admin", "user", "list", "--config", cfg); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Errorf("list before migrate: %v, want the missing table", err)
	}
	if _, _, err := run(t, nil, "admin", "user", "list", "--config", filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("list without a configuration succeeded")
	}
}

// TestAdminUserRecovery: with every admin disabled, nobody can manage the server over HTTP;
// admin user create --admin against the database makes a new admin, and set-password gives a
// disabled admin a new password, each audited with the source cli and this host (#440).
func TestAdminUserRecovery(t *testing.T) {
	c := newUserCLI(t)
	for _, name := range []string{"lost", "gone"} {
		if _, err := c.create(t, nil, strings.NewReader(name+"-password"), name, "--admin", "--password-stdin"); err != nil {
			t.Fatal(err)
		}
	}
	st := openTestStore(t, c.dsn)
	for _, name := range []string{"lost", "gone"} { // past the guard of the last admin, as by hand in the database
		u, err := st.GetUser(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		u.Disabled = true
		if err := st.UpdateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.create(t, nil, strings.NewReader("rescue-password"), "rescue", "--admin", "--password-stdin"); err != nil {
		t.Fatalf("admin user create --admin with every admin disabled: %v", err)
	}
	if _, _, err := runIn(t, nil, strings.NewReader("found-password"), "admin", "user", "set-password", "lost", "--password-stdin",
		"--config", c.cfg); err != nil {
		t.Fatal(err)
	}
	wantPassword(t, c.dsn, "rescue", "rescue-password")
	wantPassword(t, c.dsn, "lost", "found-password")
	wantCLIAudit(t, c.dsn, auth.ActionUserCreate, 3)
	wantCLIAudit(t, c.dsn, auth.ActionUserPassword, 1)
}
