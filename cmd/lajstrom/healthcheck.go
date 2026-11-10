package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/config"
)

// defaultHealthTimeout bounds one health check: connecting, the request and the answer.
const defaultHealthTimeout = 3 * time.Second

// newHealthcheckCmd returns healthcheck: one GET of /-/readyz of a running serve, or of
// /-/healthz with --live, for the health check of a container whose image has no shell and no
// HTTP client. It exits 0 when the answer is 200 and 1 otherwise, with one line either way.
// --url names the server, and no configuration is read then; without it the server is
// server.listen of the configuration, of which nothing else is read but server.tls
// (config.ServerListen): no secret, no database, no registry. With server.tls the check
// speaks https and pins the certificate of server.tls.certFile (pinnedTLS); the key is never
// read.
func newHealthcheckCmd(a *app) *cobra.Command {
	var (
		base    string
		live    bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Ask a running serve whether it is ready (/-/readyz) or up (/-/healthz with --live); exit 0 or 1",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var pinned *tls.Config
			if base == "" {
				listen, files, err := config.ServerListen(a.configPath, a.lookupEnv)
				if errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("%w (name the server with --url, or the configuration with --config)", err)
				}
				if err != nil {
					return err
				}
				if files.CertFile != "" {
					if pinned, err = pinnedTLS(files.CertFile); err != nil {
						return err
					}
				}
				base = listenURL(listen, pinned != nil)
			}
			target, err := healthURL(base, live)
			if err != nil {
				return err
			}
			line, err := healthcheck(cmd.Context(), target, timeout, pinned)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), line)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&base, "url", "", "base URL of the server, e.g. http://127.0.0.1:8080 (default: server.listen of the configuration, on loopback)")
	f.BoolVar(&live, "live", false, "ask /-/healthz, whether the process answers, instead of /-/readyz")
	f.DurationVar(&timeout, "timeout", defaultHealthTimeout, "how long to wait for the answer")
	return cmd
}

// listenURL returns the URL of a server that listens on listen, a host:port that
// config.ServerListen checked, over https when it terminates TLS itself: an address of every
// interface (":8080", "0.0.0.0:8080", "[::]:8080") is asked on loopback.
func listenURL(listen string, https bool) string {
	host, port, _ := net.SplitHostPort(listen)
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	scheme := "http://"
	if https {
		scheme = "https://"
	}
	return scheme + net.JoinHostPort(host, port)
}

// pinnedTLS returns the TLS configuration of a check of a serve that terminates TLS with the
// chain of certFile: the server must present the first certificate of that file, byte for
// byte, and nothing else of it is checked. The check goes to loopback while the certificate
// names the public host, so the system roots and the host name cannot be the test; the file
// that serve itself loaded is a stricter one. A certificate renewed on disk and not yet loaded
// fails the check, so that the orchestrator restarts serve with it.
func pinnedTLS(certFile string) (*tls.Config, error) {
	b, err := os.ReadFile(certFile) // #nosec G304 -- the operator's configuration names it
	if err != nil {
		return nil, fmt.Errorf("server.tls.certFile: %w", err)
	}
	var want []byte
	for block, rest := pem.Decode(b); block != nil; block, rest = pem.Decode(rest) {
		if block.Type == "CERTIFICATE" {
			want = block.Bytes
			break
		}
	}
	if want == nil {
		return nil, fmt.Errorf("server.tls.certFile %s: no certificate", certFile)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The chain and the name are not checked against roots: VerifyConnection compares the
		// certificate with the pinned one instead, on every handshake.
		InsecureSkipVerify: true, //nolint:gosec // the certificate is pinned by VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, want) {
				return errors.New("the server's certificate is not the first one of server.tls.certFile")
			}
			return nil
		},
	}, nil
}

// healthURL returns the URL of /-/readyz, or of /-/healthz with live, under base, an absolute
// http(s) URL with no path but "/", no query and no credentials.
func healthURL(base string, live bool) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("--url: %q is not a server's base URL, such as http://127.0.0.1:8080", base)
	}
	u.Path = "/-/readyz"
	if live {
		u.Path = "/-/healthz"
	}
	return u.String(), nil
}

// healthcheck GETs target once, within timeout, without a proxy, a redirect or a kept
// connection, and returns "<target>: <status> <first line of the body>" when the answer is
// 200; any other answer, or none, is an error of the same form that starts with "unhealthy".
// tlsConfig, when not nil, is the TLS of an https target; nil is the system's roots.
func healthcheck(ctx context.Context, target string, timeout time.Duration, tlsConfig *tls.Config) (string, error) {
	client := &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSClientConfig: tlsConfig},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", fmt.Errorf("unhealthy: %s: %w", target, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Timeout() {
			return "", fmt.Errorf("unhealthy: %s: no answer within %s", target, timeout)
		}
		if errors.As(err, &ue) {
			err = ue.Err // the url.Error repeats the method and the URL
		}
		return "", fmt.Errorf("unhealthy: %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	line := firstLine(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unhealthy: %s: %d %s", target, resp.StatusCode, line)
	}
	return fmt.Sprintf("%s: %d %s", target, resp.StatusCode, line), nil
}

// firstLine returns the first line of r, at most 200 bytes of it read, with anything that is
// not a printable character dropped, so that an answer cannot write to the terminal.
func firstLine(r io.Reader) string {
	line, _ := bufio.NewReader(io.LimitReader(r, 200)).ReadString('\n')
	return strings.Map(func(c rune) rune {
		if unicode.IsPrint(c) {
			return c
		}
		return -1
	}, strings.TrimSpace(line))
}
