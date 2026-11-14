package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// repoLabel is the grammar of the repo label of a repo host: a repo name (config.RepoNamePattern)
// that is also a DNS label, letters, digits and single hyphens, at most maxRepoLabel bytes. A
// repo whose name has a . or a _, or is 64 bytes long, has no host (docs/spec/http-routing.md
// §1.1).
var repoLabel = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// maxRepoLabel is the longest DNS label, in bytes (RFC 1035).
const maxRepoLabel = 63

// fqdn is the FQDN mode of a server (docs/spec/http-routing.md §1.1): every repo also has a host
// below domain, whose requests are served as the path form of the repo.
type fqdn struct {
	domain   string   // server.fqdn.domain, lower case
	mainHost string   // the host of server.externalURL, lower case: never a repo host
	scheme   string   // the scheme of server.externalURL, the scheme of a repo host's URLs
	port     string   // the port of server.externalURL, "" for none, the port of a repo host's URLs
	base     string   // server.externalURL without a trailing /
	formats  []string // the formats served, in the order of Options.Plugins
}

// newFQDN returns the FQDN mode of domain, nil when domain is empty. The mode needs externalURL:
// the URLs of a repo host take its scheme and port. config.Validate checks domain itself.
func newFQDN(domain, externalURL string, plugins []format.Plugin) (*fqdn, error) {
	if domain == "" {
		return nil, nil //nolint:nilnil // no FQDN mode is not an error
	}
	u, err := url.Parse(externalURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("server: the FQDN mode needs ExternalURL: the URLs of a repo host take its scheme and port")
	}
	f := &fqdn{domain: strings.ToLower(domain), mainHost: strings.ToLower(u.Hostname()), scheme: u.Scheme, port: u.Port(),
		base: strings.TrimSuffix(externalURL, "/")}
	for _, p := range plugins {
		f.formats = append(f.formats, p.Name())
	}
	return f, nil
}

// hostMatch is what the Host of a request is in the FQDN mode.
type hostMatch struct {
	host   string // the Host, lower case, without its port and a trailing dot
	below  bool   // below the domain and not the main host: a repo host or nothing
	label  string // the repo label
	format string // the format label of <repo>.<format>.<domain>; "" for <repo>.<domain>
	why    string // why the host is no repo host, for the log; "" when label may name a repo
}

// match reads the Host of a request. The host of server.externalURL, the domain and every host
// not below it are the main host; a host below the domain is <repo>.<domain> or
// <repo>.<format>.<domain>, with <format> a format the server serves and <repo> a repoLabel, or
// nothing. An IPv6 literal is never below a domain.
func (f *fqdn) match(hostport string) hostMatch {
	host := hostport
	if strings.HasPrefix(host, "[") {
		return hostMatch{host: host}
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	m := hostMatch{host: host}
	sub, ok := strings.CutSuffix(host, "."+f.domain)
	if !ok || host == f.mainHost {
		return m
	}
	m.below = true
	switch labels := strings.Split(sub, "."); {
	case len(labels) == 1:
		m.label = labels[0]
	case len(labels) == 2 && slices.Contains(f.formats, labels[1]):
		m.label, m.format = labels[0], labels[1]
	case len(labels) == 2:
		m.why = fmt.Sprintf("%q is not a format the server serves", labels[1])
		return m
	default:
		m.why = "more labels below the domain than <repo>.<format>"
		return m
	}
	if len(m.label) > maxRepoLabel || !repoLabel.MatchString(m.label) {
		m.why = fmt.Sprintf("%q is not a repo name that is a DNS label", m.label)
	}
	return m
}

// resolve returns the repo the host of m names: of <repo>.<format>.<domain>, the repo of that
// format; of <repo>.<domain>, the one repo of that name among the formats served. why says, for
// the log, why the host names none: no such repo, or repos of several formats with the name. err
// is the index's error. idx is not used when the server serves no format (New needs an index
// for plugins).
func (f *fqdn) resolve(ctx context.Context, idx index.Reader, m hostMatch) (repo index.Repo, why string, err error) {
	if m.why != "" {
		return index.Repo{}, m.why, nil
	}
	formats := f.formats
	if m.format != "" {
		formats = []string{m.format}
	}
	var found []index.Repo
	for _, name := range formats {
		r, err := idx.GetRepo(ctx, name, m.label)
		switch {
		case errors.Is(err, index.ErrNotFound) || (err == nil && r == nil):
			continue
		case err != nil:
			return index.Repo{}, "", fmt.Errorf("server: the repo of the host %s: %w", m.host, err)
		}
		found = append(found, *r)
	}
	switch len(found) {
	case 0:
		return index.Repo{}, "no repo of that name", nil
	case 1:
		return found[0], "", nil
	}
	names := make([]string, len(found))
	for i, r := range found {
		names[i] = r.Format + "/" + r.Name
	}
	return index.Repo{}, "repos of several formats have the name: " + strings.Join(names, ", "), nil
}

// repoHost is the RepoHost of a request on the host of m that names repo. Its URL is built from
// the repo's name, the form of m, the domain, and the scheme and the port of server.externalURL;
// nothing of it comes from the request.
func (f *fqdn) repoHost(m hostMatch, repo index.Repo) RepoHost {
	host := repo.Name + "." + f.domain
	if m.format != "" {
		host = repo.Name + "." + repo.Format + "." + f.domain
	}
	if f.port != "" {
		host = net.JoinHostPort(host, f.port)
	}
	return RepoHost{Format: repo.Format, Repo: repo.Name, URL: f.scheme + "://" + host + "/",
		PathURL: f.base + "/" + repo.Format + "/" + repo.Name + "/"}
}

// noRepo is the message of a host below the domain that names no repo.
func (f *fqdn) noRepo(host string) string {
	return fmt.Sprintf("no repo is served at the host %s: a repo host is <repo>.%s, of a repo whose name no repo of another "+
		"format has, or <repo>.<format>.%s; a repo whose name has a . or a _, or is 64 bytes long, has none, and is served at "+
		"%s/<format>/<repo>/", host, f.domain, f.domain, f.base)
}

// covers reports whether the certificate of c covers the repo hosts <repo>.<domain>, or there
// is no certificate to tell.
func (f *fqdn) covers(c *tls.Config) bool {
	if c == nil || len(c.Certificates) == 0 || len(c.Certificates[0].Certificate) == 0 {
		return true
	}
	leaf := c.Certificates[0].Leaf
	if leaf == nil {
		var err error
		if leaf, err = x509.ParseCertificate(c.Certificates[0].Certificate[0]); err != nil {
			return true
		}
	}
	return leaf.VerifyHostname("repo."+f.domain) == nil
}

// logFQDN logs the FQDN mode at start, and warns when server.tls has a certificate that does not
// cover the repo hosts: Lajstrom sends its one certificate whatever the SNI name.
func (s *Server) logFQDN() {
	if s.fqdn == nil {
		return
	}
	s.log.Info("FQDN mode: every repo also has the host <repo>.<domain>", "domain", s.fqdn.domain)
	if !s.fqdn.covers(s.srv.TLSConfig) {
		s.log.Warn("server.tls: the certificate does not cover the repo hosts *.<domain>, and clients of a repo host will refuse it: "+
			"give it the names of the main host and *.<domain>", "domain", s.fqdn.domain)
	}
}

// hostRefusal is a request on a host below the domain that names no repo; refuseHost answers it.
type hostRefusal struct {
	host string
	err  error // index.ErrNotFound when the host names no repo; the index's error otherwise
}

// hostRefusalKey is the context key of the hostRefusal of a request.
type hostRefusalKey struct{}

// repoHosts is the FQDN mode (docs/spec/http-routing.md §1.1). A request whose Host is a repo
// host is rewritten to the path form of the repo, /<format>/<repo><path>, before it is
// authenticated and routed: every path, /-/ and /v2/ included, so that npm's own routes stay
// the repo's. It carries its RepoHost (RepoHostFrom), and a Location below the path form in its
// answer is written below the repo host (hostWriter). A request on a host below the domain that
// names no repo goes on unchanged with a hostRefusal, which refuseHost answers once the request
// is authenticated. Every other host is the main host, and its requests go on as they came.
func (s *Server) repoHosts(next http.Handler) http.Handler {
	if s.fqdn == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := s.fqdn.match(r.Host)
		if !m.below {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		repo, why, err := s.fqdn.resolve(ctx, s.opts.Deps.Index, m)
		if err != nil || why != "" {
			if why != "" {
				s.log.DebugContext(ctx, "no repo at the host", "host", m.host, "reason", why, "requestId", RequestID(ctx))
				err = index.ErrNotFound
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, hostRefusalKey{}, hostRefusal{host: m.host, err: err})))
			return
		}
		h := s.fqdn.repoHost(m, repo)
		prefix := "/" + repo.Format + "/" + repo.Name
		u := *r.URL
		u.Path = prefix + u.Path
		if u.RawPath != "" {
			u.RawPath = prefix + u.RawPath
		}
		r = r.WithContext(WithRepoHost(ctx, h))
		r.URL = &u
		next.ServeHTTP(&hostWriter{ResponseWriter: w, prefix: prefix, pathURL: h.PathURL, url: h.URL}, r)
	})
}

// refuseHost answers a request of repoHosts on a host below the domain that names no repo, once
// it is authenticated: 404, and 401 with the Basic challenge to the anonymous principal when the
// format plugins authorize, as on a repo it may not read (docs/spec/rbac.md §3), so that the
// answer does not tell an unknown repo from a private one. A lookup the index failed is its
// 5xx, logged.
func (s *Server) refuseHost(next http.Handler) http.Handler {
	if s.fqdn == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ref, ok := ctx.Value(hostRefusalKey{}).(hostRefusal)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		err := ref.err
		switch {
		case !errors.Is(err, index.ErrNotFound):
		case s.opts.Deps.Auth != nil && auth.PrincipalFrom(ctx).Kind == auth.KindAnonymous:
			err = fmt.Errorf("%w: the host %s", format.ErrUnauthenticated, ref.host)
		default:
			err = fmt.Errorf("%w: %s", index.ErrNotFound, s.fqdn.noRepo(ref.host))
		}
		if status := format.WriteError(w, err); status >= http.StatusInternalServerError {
			s.log.Log(ctx, FailLevel(r), "repo host lookup failed", "host", ref.host, "status", status, "error", err.Error(),
				"requestId", RequestID(ctx))
		}
	})
}

// hostWriter is the http.ResponseWriter of a request on a repo host. When the header is sent, a
// Location below the path form of the repo, /<format>/<repo>/… (a redirect of a frontend or of
// the ServeMux, the Location of an upload) or <externalURL>/<format>/<repo>/…, is written below
// the repo host: /… and <repo host URL>…. It keeps http.Flusher working and unwraps for
// http.ResponseController.
type hostWriter struct {
	http.ResponseWriter
	prefix       string // /<format>/<repo>
	pathURL, url string // RepoHost.PathURL and RepoHost.URL
	sent         bool   // the final header is sent
}

// WriteHeader rewrites the Location before the first final header; 1xx headers are not final.
func (w *hostWriter) WriteHeader(code int) {
	if !w.sent {
		w.location()
		w.sent = code >= http.StatusOK
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write sends the header with its Location rewritten, when it is not sent yet, and writes b.
func (w *hostWriter) Write(b []byte) (int, error) {
	if !w.sent {
		w.location()
		w.sent = true
	}
	return w.ResponseWriter.Write(b)
}

// Flush sends buffered data to the client, when the underlying writer can.
func (w *hostWriter) Flush() {
	if !w.sent {
		w.location()
		w.sent = true
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying writer for http.ResponseController.
func (w *hostWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// location writes the Location of the header below the repo host, when it is below the path
// form of the repo.
func (w *hostWriter) location() {
	h := w.Header()
	loc := h.Get("Location")
	if rest, ok := strings.CutPrefix(loc, w.pathURL); ok {
		h.Set("Location", w.url+rest)
		return
	}
	rest, ok := strings.CutPrefix(loc, w.prefix)
	if !ok || (rest != "" && rest[0] != '/' && rest[0] != '?') {
		return
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	h.Set("Location", rest)
}
