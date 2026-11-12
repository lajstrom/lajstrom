package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
)

// bootstrapTimeout bounds the request of admin bootstrap; the server hashes the password.
const bootstrapTimeout = 30 * time.Second

// newAdminBootstrapCmd returns admin bootstrap: exchange the bootstrap token for the first
// admin, over HTTP, with POST /api/v1/bootstrap (docs/auth.md, Bootstrap). It needs no database
// and no credentials: the token is the credential. --server names the server; without it the
// server is server.listen of the configuration on loopback, as for healthcheck, of which
// nothing else is read. The token comes from --token-file, else from a prompt without echo; the
// password from standard input with --password-stdin, else from a prompt, twice. It prints the
// admin as JSON on one line.
func newAdminBootstrapCmd(a *app) *cobra.Command {
	var (
		server, name, tokenFile string
		useStdin                bool
	)
	cmd := &cobra.Command{
		Use:   "bootstrap --name <name> [--server <url>] [--token-file <file>] [--password-stdin]",
		Short: "Create the first admin with the bootstrap token, over HTTP; print it as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := auth.CheckName(name); err != nil {
				return err
			}
			base, tlsConfig, err := a.bootstrapServer(server)
			if err != nil {
				return err
			}
			if tokenFile == "" && useStdin {
				return errors.New("--password-stdin needs --token-file: standard input holds the password, the token cannot be prompted for")
			}
			token, err := readBootstrapToken(cmd, tokenFile)
			if err != nil {
				return err
			}
			password, err := a.readPassword(cmd, useStdin)
			if err != nil {
				return err
			}
			body, err := bootstrap(cmd.Context(), base, tlsConfig, api.BootstrapRequest{Token: token, Name: name, Password: password})
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(body)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "the admin's user name")
	f.StringVar(&server, "server", "", "base URL of the server, e.g. https://art.example.com (default: server.listen of the configuration, on loopback)")
	f.StringVar(&tokenFile, "token-file", "", "read the bootstrap token from this file (default: a prompt on the terminal)")
	f.BoolVar(&useStdin, "password-stdin", false, "read the password from standard input, without the trailing newline")
	_ = cmd.MarkFlagRequired("name") // fails only for a flag that does not exist
	return cmd
}

// bootstrapServer returns the base URL of the server, base when it is set, and the TLS of the
// request: nil for the system's roots, or the pin of server.tls.certFile for the server of
// the configuration (pinnedTLS). A configuration that cannot be read or is invalid is a
// configError, exit status 2, as for the other commands; healthcheck alone exits 1 for it.
func (a *app) bootstrapServer(base string) (string, *tls.Config, error) {
	if base != "" {
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			strings.TrimSuffix(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
			return "", nil, fmt.Errorf("--server: %q is not a server's base URL, such as https://art.example.com", base)
		}
		return u.Scheme + "://" + u.Host, nil, nil
	}
	listen, files, err := config.ServerListen(a.configPath, a.lookupEnv)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, configError{fmt.Errorf("%w (name the server with --server, or the configuration with --config)", err)}
	}
	if err != nil {
		return "", nil, configError{err}
	}
	if files.CertFile == "" {
		return listenURL(listen, false), nil, nil
	}
	pinned, err := pinnedTLS(files.CertFile)
	if err != nil {
		return "", nil, err
	}
	return listenURL(listen, true), pinned, nil
}

// readBootstrapToken returns the token of file without the whitespace around it, or the one
// typed at a prompt without echo when file is "".
func readBootstrapToken(cmd *cobra.Command, file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file) // #nosec G304 -- the operator names the file
		if err != nil {
			return "", fmt.Errorf("--token-file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok || !terminal.isTerminal(int(f.Fd())) {
		return "", errors.New("no bootstrap token: pass --token-file, or run on a terminal to be prompted")
	}
	token, err := promptPassword(cmd.ErrOrStderr(), int(f.Fd()), "Bootstrap token: ")
	return strings.TrimSpace(token), err
}

// bootstrap POSTs req to /api/v1/bootstrap under base, without following a redirect, and
// returns the body of the 201, the admin as JSON on one line. Any other answer is an error
// with its status and the message of its envelope; a 404 says what it means.
func bootstrap(ctx context.Context, base string, tlsConfig *tls.Config, req api.BootstrapRequest) ([]byte, error) {
	payload, err := json.Marshal(req) // #nosec G117 -- the password goes to the server that makes the admin
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, bootstrapTimeout)
	defer cancel()
	target := base + "/api/v1/bootstrap"
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsConfig},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(r)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("bootstrap: POST %s: %w", target, err)
	}
	if resp.StatusCode == http.StatusCreated {
		return append(bytes.TrimSpace(body), '\n'), nil
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := firstLine(bytes.NewReader(body))
	if json.Unmarshal(body, &env) == nil && env.Error.Message != "" {
		msg = env.Error.Message
	}
	if resp.StatusCode == http.StatusNotFound {
		msg = "an admin exists already, or this is not a server with the bootstrap; " +
			"with the database, lajstrom admin user create --admin still makes one"
	}
	return nil, fmt.Errorf("bootstrap: POST %s: %d %s", target, resp.StatusCode, msg)
}
