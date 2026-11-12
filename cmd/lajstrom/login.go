package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// loginRequestTimeout bounds each request of login to the server.
const loginRequestTimeout = 30 * time.Second

// deviceGrantType is the grant type of the device flow (RFC 8628 §3.4).
const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// newLoginCmd returns login: the device flow of the server's OIDC login (docs/spec/rbac.md §8,
// docs/auth.md). It asks the server for a device request, tells the person on standard error
// which page to open and which code to confirm, polls until the login in the browser finished,
// and prints the personal access token of the login on standard output: one JSON line, or the
// token alone with --output token, for docker login --password-stdin. It needs no configuration
// and no credentials, and writes no file: the token goes where the person puts it.
func newLoginCmd(a *app) *cobra.Command {
	var server, output string
	cmd := &cobra.Command{
		Use:   "login --server <url> [--output json|token]",
		Short: "Log in with the server's OIDC identity provider from a terminal; print a personal access token",
		Long: "login runs the device flow of the server's OIDC login: open the page it names in a browser, on any\n" +
			"machine, confirm the code it shows and log in at your identity provider. It then prints a personal\n" +
			"access token of your user, which every client takes as a password (docs/auth.md, OIDC).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if output != "json" && output != "token" {
				return fmt.Errorf("--output: %q is not json or token", output)
			}
			base, _, err := a.bootstrapServer(server)
			if err != nil {
				return err
			}
			res, err := deviceLogin(cmd.Context(), &http.Client{Timeout: loginRequestTimeout}, base, cmd.ErrOrStderr(), sleepCtx)
			if err != nil {
				return err
			}
			host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Logged in to %s as %s; the token expires at %s.\n"+
				"For docker: lajstrom login --server %s --output token | docker login %s -u %s --password-stdin\n",
				host, res.User, res.ExpiresAt.Format(time.RFC3339), base, host, res.User)
			out := cmd.OutOrStdout()
			if output == "token" {
				_, err = fmt.Fprintln(out, res.Token)
				return err
			}
			return json.NewEncoder(out).Encode(res)
		},
	}
	f := cmd.Flags()
	f.StringVar(&server, "server", "", "base URL of the server, e.g. https://art.example.com (default: server.listen of the configuration, on loopback)")
	f.StringVar(&output, "output", "json", "json: one line with the server, the user, the token and its expiry; token: the token alone")
	return cmd
}

// loginResult is what login prints.
type loginResult struct {
	Server    string    `json:"server"`
	User      string    `json:"user"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// deviceStart is the server's answer to POST /-/oidc/device/code (RFC 8628 §3.2).
type deviceStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// devicePoll is the server's answer to POST /-/oidc/token: the token, or an error of RFC 8628 §3.5.
type devicePoll struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int    `json:"expires_in"`
	User             string `json:"user"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// deviceLogin runs the device flow against the server at base: it starts a device request,
// writes the page and the code to msgs, and polls at the server's interval, five seconds more
// at each slow_down, until the login is approved, refused or expired. sleep waits between the
// polls; the tests pass one that does not.
func deviceLogin(ctx context.Context, c *http.Client, base string, msgs io.Writer,
	sleep func(context.Context, time.Duration) error) (*loginResult, error) {
	var d deviceStart
	status, err := postForm(ctx, c, base+"/-/oidc/device/code", url.Values{"client_id": {"lajstrom-cli"}}, &d)
	switch {
	case err != nil:
		return nil, err
	case status == http.StatusNotFound:
		return nil, fmt.Errorf("%s has no OIDC login (auth.oidc is off)", base)
	case status != http.StatusOK || d.DeviceCode == "":
		return nil, fmt.Errorf("%s did not start a device login: %d", base, status)
	}
	page := d.VerificationURIComplete
	if page == "" {
		page = d.VerificationURI
	}
	_, _ = fmt.Fprintf(msgs, "To log in, open this page in a browser and confirm the code %s:\n\n    %s\n\nWaiting for the login (%d minutes)...\n",
		d.UserCode, page, max(d.ExpiresIn/60, 1))
	interval := time.Duration(max(d.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(d.ExpiresIn) * time.Second)
	for {
		if err := sleep(ctx, interval); err != nil {
			return nil, err
		}
		res, slower, err := poll(ctx, c, base, d.DeviceCode)
		switch {
		case err != nil:
			return nil, err
		case res != nil:
			return res, nil
		case slower:
			interval += 5 * time.Second
		}
		if time.Now().After(deadline) {
			return nil, errors.New("the code expired before the login finished: run lajstrom login again")
		}
	}
}

// poll polls the device request of deviceCode once: the result once the login is approved, slower
// for slow_down, nothing while it waits, an error when it is refused or expired.
func poll(ctx context.Context, c *http.Client, base, deviceCode string) (res *loginResult, slower bool, err error) {
	var p devicePoll
	status, err := postForm(ctx, c, base+"/-/oidc/token", url.Values{"grant_type": {deviceGrantType}, "device_code": {deviceCode}}, &p)
	switch {
	case err != nil:
		return nil, false, err
	case status == http.StatusOK && p.AccessToken != "":
		return &loginResult{Server: base, User: p.User, Token: p.AccessToken,
			ExpiresAt: time.Now().Add(time.Duration(p.ExpiresIn) * time.Second).UTC().Truncate(time.Second)}, false, nil
	case p.Error == "authorization_pending":
		return nil, false, nil
	case p.Error == "slow_down":
		return nil, true, nil
	case p.Error == "access_denied":
		return nil, false, errors.New("the login was refused in the browser: run lajstrom login again")
	case p.Error == "expired_token":
		return nil, false, errors.New("the code expired before the login finished: run lajstrom login again")
	}
	return nil, false, fmt.Errorf("the server refused the login: %d %s %s", status, p.Error, p.ErrorDescription)
}

// postForm posts form to u and decodes the JSON answer into v, whatever its status, which it
// returns. An answer that is not JSON is an error naming the status.
func postForm(ctx context.Context, c *http.Client, u string, form url.Values, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(v); err != nil {
		return resp.StatusCode, fmt.Errorf("%s answered %s, not JSON", u, resp.Status)
	}
	return resp.StatusCode, nil
}

// sleepCtx waits d, or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
