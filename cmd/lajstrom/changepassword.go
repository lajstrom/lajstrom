package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
)

// changePasswordTimeout bounds the request of change-password; the server verifies the current
// password and compares the new one with the last four, argon2id each.
const changePasswordTimeout = 30 * time.Second

// newChangePasswordCmd returns change-password: a user's own change of its password on a server,
// POST /api/v1/users/<name>/password with the current password, which is the credential of the
// request (docs/spec/rbac.md §1, A user's own password; docs/auth.md). Like login it is a client of
// the server and needs no database and no other credential: --server names the server; without
// it, the server is server.listen of the configuration on loopback, with the pin of
// server.tls.certFile, as for admin bootstrap. On a terminal it prompts for the current password
// and the new one, twice, without echo; otherwise it reads the current password from the first
// line of standard input and the new one from the second. No password is ever a flag.
func newChangePasswordCmd(a *app) *cobra.Command {
	var server string
	cmd := &cobra.Command{
		Use:   "change-password <name> [--server <url>]",
		Short: "Change your own password on a server, with your current password",
		Long: "change-password changes the password of the user <name> on a server, with its current password, which\n" +
			"is the credential of the request: it works when the password must be changed before anything else\n" +
			"(auth.passwords.changeAtFirstUse). On a terminal it prompts for the current password and the new one,\n" +
			"without echo; otherwise it reads the current password from the first line of standard input and the\n" +
			"new one from the second. The new password follows the server's rules and is none of the last four\n" +
			"(docs/auth.md, Password rules, your own password, change at first use).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := auth.CheckName(name); err != nil {
				return err
			}
			base, tlsConfig, err := a.bootstrapServer(server)
			if err != nil {
				return err
			}
			current, password, err := readPasswordChange(cmd)
			if err != nil {
				return err
			}
			if err := changePassword(cmd.Context(), base, tlsConfig, name, current, password); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.ErrOrStderr(), "The password of %s is changed.\n", name)
			return err
		},
	}
	cmd.Flags().StringVar(&server, "server", "",
		"base URL of the server, e.g. https://art.example.com (default: server.listen of the configuration, on loopback)")
	return cmd
}

// readPasswordChange returns the current password and the new one of change-password: on a
// terminal, prompts without echo, the new one twice; otherwise the first and the second line of
// standard input, each without its LF or CR LF, and nothing after them.
func readPasswordChange(cmd *cobra.Command) (current, password string, err error) {
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok && terminal.isTerminal(int(f.Fd())) {
		fd, prompt := int(f.Fd()), cmd.ErrOrStderr()
		if current, err = promptPassword(prompt, fd, "Current password: "); err != nil {
			return "", "", err
		}
		if password, err = promptPassword(prompt, fd, "New password: "); err != nil {
			return "", "", err
		}
		again, err := promptPassword(prompt, fd, "Repeat the new password: ")
		switch {
		case err != nil:
			return "", "", err
		case again != password:
			return "", "", errors.New("the new passwords do not match")
		}
		return current, password, nil
	}
	b, err := io.ReadAll(io.LimitReader(in, 2*(auth.MaxPasswordLen+2)+1)) // two passwords, two CR LF, one byte to be too long
	if err != nil {
		return "", "", fmt.Errorf("read the passwords from standard input: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 2 {
		return "", "", errors.New("standard input: want the current password on the first line and the new one on the second, " +
			"and nothing else; or run on a terminal to be prompted")
	}
	current, password = strings.TrimSuffix(lines[0], "\r"), strings.TrimSuffix(lines[1], "\r")
	if current == "" || password == "" {
		return "", "", errors.New("standard input: a line is empty: want the current password on the first line and the new one on the second")
	}
	return current, password, nil
}

// changePassword POSTs the user's own change to /api/v1/users/<name>/password under base, without
// following a redirect: nil for the 204, else an error with the status and the message of the
// answer's envelope, and for a 429 the seconds of its Retry-After.
func changePassword(ctx context.Context, base string, tlsConfig *tls.Config, name, current, password string) error {
	payload, err := json.Marshal(api.PasswordRequest{Password: password, CurrentPassword: &current}) // #nosec G117 -- the passwords go to the server that checks them
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, changePasswordTimeout)
	defer cancel()
	target := base + "/api/v1/users/" + url.PathEscape(name) + "/password"
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsConfig},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(r)
	if err != nil {
		return fmt.Errorf("change-password: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("change-password: POST %s: %w", target, err)
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	msg := answerMessage(body)
	if resp.StatusCode == http.StatusTooManyRequests && resp.Header.Get("Retry-After") != "" {
		msg += " (Retry-After: " + resp.Header.Get("Retry-After") + "s)"
	}
	return fmt.Errorf("change-password: POST %s: %d %s", target, resp.StatusCode, msg)
}
