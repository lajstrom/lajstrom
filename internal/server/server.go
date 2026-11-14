package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// DefaultShutdownTimeout is the graceful shutdown bound when Options.ShutdownTimeout is zero.
const DefaultShutdownTimeout = 30 * time.Second

// abortGrace is how long Run waits, after the shutdown timeout, for the handlers it cancelled
// to return; a variable so that tests can shorten it.
var abortGrace = 5 * time.Second

// Limits of the http.Server.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 1 << 20
)

// Options configures a Server.
type Options struct {
	// Listen is the TCP address to listen on, host:port. Port 0 picks a free port; Addr reports it.
	Listen string
	// ExternalURL is the base of absolute URLs (server.externalURL); empty when not configured.
	ExternalURL string
	// TLSCertFile and TLSKeyFile are the PEM certificate chain and key of the main listener
	// (server.tls); New loads them, and Run serves TLS. Both empty: plain HTTP, TLS at the edge.
	TLSCertFile, TLSKeyFile string
	// ShutdownTimeout bounds the graceful shutdown; zero means DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
	// ShutdownDelay is how long the server keeps serving once Run's context ends, with
	// /-/readyz answering 503 and every connection closed after its next response, before the
	// listener closes and the drain of ShutdownTimeout begins: the time an edge needs to stop
	// sending it new requests. Zero closes the listener at once.
	ShutdownDelay time.Duration
	// OnShutdown is called when the listener closes, after ShutdownDelay, and returns before Run
	// waits for the requests in flight; nil means nothing.
	OnShutdown func()
	// Log receives the access log and the server's own events; nil means slog.Default().
	Log *slog.Logger
	// Plugins are the format plugins to serve, normally format.All(). Their names must pass
	// format.CheckName and be distinct.
	Plugins []format.Plugin
	// Deps are the services the format plugins get. Deps.Index is required when Plugins is not
	// empty. Each plugin gets Deps.Log, or the server's Log when it is nil, with the attribute
	// format=<name>.
	Deps format.Deps
	// Ready reports whether the server can take traffic: the index database is reachable and
	// the registries answer (docs/spec/http-routing.md §5). nil means always ready.
	Ready func(ctx context.Context) error
	// API is the management REST API (internal/api), mounted at /api/. nil leaves /api/ to the
	// ServeMux, which answers it 404 in plain text.
	API http.Handler
	// Facade is the OCI distribution API (internal/facade), mounted at /v2/. nil leaves /v2/ to
	// the ServeMux, which answers it 404 in plain text.
	Facade http.Handler
	// OIDC is the OIDC login (internal/auth/oidc, docs/spec/http-routing.md §7), mounted at
	// /-/oidc/ while auth.oidc is on. nil leaves /-/oidc/ to the ServeMux, which answers it 404.
	// The authentication middleware lets its requests through as anonymous, whatever their
	// Authorization header says.
	OIDC http.Handler
	// Authenticator turns the HTTP Basic credentials of a request, and a personal access token
	// as a bearer outside /v2, into its principal and credential, on every route
	// (authenticate). nil authenticates nobody: every request is anonymous.
	Authenticator *auth.Authenticator
	// MetricsListen is the address of the metrics listener, host:port (server.metricsListen):
	// GET /metrics, and /debug/pprof/ with Pprof. Empty means no metrics listener. The main
	// listener never serves them (docs/spec/http-routing.md §5).
	MetricsListen string
	// Pprof serves /debug/pprof/ on the metrics listener (server.pprof).
	Pprof bool
	// StallTimeout is how long a request body or a response may make no progress before the
	// connection is closed (server.stallTimeout); zero means no bound.
	StallTimeout time.Duration
	// MaxUploads is how many uploads (POST, PUT, PATCH outside /api/, /-/ and
	// /v2/token) may run at once; the next one is answered 503 (server.maxConcurrentUploads).
	// Zero means no limit.
	MaxUploads int
	// TrustedProxies are the peers whose X-Forwarded-For names the client
	// (server.trustedProxies, ADR 0011). Empty: the peer of the connection is the client.
	TrustedProxies []netip.Prefix
	// FQDNDomain turns on the FQDN mode (server.fqdn.domain, docs/spec/http-routing.md §1.1):
	// every repo also has the host <repo>.<domain>, and <repo>.<format>.<domain>, whose requests
	// are served as the path form of the repo (repoHosts). It needs ExternalURL. Empty: off.
	FQDNDomain string
}

// Server is the Lajstrom HTTP server. Create it with New and start it with Run.
type Server struct {
	opts    Options
	log     *slog.Logger
	mux     *http.ServeMux
	handler http.Handler
	srv     *http.Server
	// byAddress is whether the client address counts in the per-address throttles: Lajstrom
	// knows the client behind trusted proxies, or when it terminates TLS itself (clientAddress).
	byAddress bool
	// fqdn is the FQDN mode; nil when it is off.
	fqdn *fqdn

	running     atomic.Bool
	draining    atomic.Bool  // set when Run's context ends; /-/readyz answers 503 from then on
	inflight    atomic.Int64 // handlers running
	abort       context.CancelCauseFunc
	baseCtx     context.Context // the parent of every request's context; abort cancels it (errAborted)
	bound       chan struct{}   // closed when Run has bound the listeners, or failed to
	mu          sync.Mutex
	addr        net.Addr
	metricsAddr net.Addr
}

// New validates o, builds the routes and the middleware chain, and returns a Server that is not
// listening yet. It returns an error naming the plugin when a plugin is nil, its name fails
// format.CheckName or is taken twice, and an error when there are plugins but no Deps.Index.
func New(o Options) (*Server, error) {
	if o.Listen == "" {
		return nil, errors.New("server: Listen must not be empty")
	}
	if err := checkPlugins(o.Plugins); err != nil {
		return nil, err
	}
	if len(o.Plugins) > 0 && o.Deps.Index == nil {
		return nil, errors.New("server: format plugins need Deps.Index")
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}
	tlsConfig, err := loadTLS(o.TLSCertFile, o.TLSKeyFile)
	if err != nil {
		return nil, err
	}
	hosts, err := newFQDN(o.FQDNDomain, o.ExternalURL, o.Plugins)
	if err != nil {
		return nil, err
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	s := &Server{opts: o, log: o.Log, mux: http.NewServeMux(), bound: make(chan struct{}),
		byAddress: len(o.TrustedProxies) > 0 || tlsConfig != nil, fqdn: hosts}
	s.baseCtx, s.abort = context.WithCancelCause(context.Background())
	s.routes()
	s.handler = s.track(s.securityHeaders(s.recovery(requestID(s.clientAddress(s.repoHosts(s.accessLog(s.crossOrigin(
		s.authenticate(s.refuseHost(s.limitUploads(s.stall(s.mux))))))))))))
	s.srv = &http.Server{
		Handler:     s.handler,
		BaseContext: func(net.Listener) context.Context { return s.baseCtx },
		// No ReadTimeout and no WriteTimeout: they bound the whole request body and the whole
		// response, and a blob upload or download streams for as long as its size needs. Slow
		// clients are bounded by ReadHeaderTimeout and IdleTimeout, and a body that stops
		// moving by StallTimeout (stall).
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(o.Log.Handler(), slog.LevelWarn),
		TLSConfig:         tlsConfig,
	}
	return s, nil
}

// loadTLS loads the certificate of the main listener, or returns nil when certFile and keyFile
// are both empty. TLS 1.2 is the oldest version it speaks; the cipher suites and curves are Go's
// defaults, and HTTP/2 is offered.
func loadTLS(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil //nolint:nilnil // no TLS is not an error
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("server: tls: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, nil
}

// Handler returns the server's handler: the routes behind the middleware chain.
func (s *Server) Handler() http.Handler { return s.handler }

// Addr returns the address the server listens on, with the actual port when Listen asked for
// port 0. It is nil until Run has bound the listener.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// MetricsAddr returns the address of the metrics listener, with the actual port when
// MetricsListen asked for port 0. It is nil until Run has bound it, and without MetricsListen.
func (s *Server) MetricsAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metricsAddr
}

// Run listens on Options.Listen and serves until ctx is cancelled. Then it drains: /-/readyz
// answers 503, idle connections are closed and the others after their next response, and it
// keeps serving for ShutdownDelay; then the listener closes, and in-flight requests, uploads
// included, get up to ShutdownTimeout to finish. Requests still running at the deadline are
// aborted: their contexts are cancelled and their connections closed, and Run waits up to
// abortGrace for their handlers to return, so that what serve closes after Run (the index) is
// not closed under them. It returns nil after a clean stop, and an error when it cannot listen,
// when serving fails, or when requests were aborted. Run may be called once.
func (s *Server) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("server: Run called twice")
	}
	defer s.abort(errAborted)
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.opts.Listen)
	if err != nil {
		close(s.bound)
		return fmt.Errorf("server: %w", err)
	}
	var metricsLn net.Listener
	if s.opts.MetricsListen != "" {
		if metricsLn, err = lc.Listen(ctx, "tcp", s.opts.MetricsListen); err != nil {
			_ = ln.Close()
			close(s.bound)
			return fmt.Errorf("server: metrics listener: %w", err)
		}
	}
	s.mu.Lock()
	s.addr = ln.Addr()
	if metricsLn != nil {
		s.metricsAddr = metricsLn.Addr()
	}
	s.mu.Unlock()
	close(s.bound)
	s.log.Info("listening", "addr", ln.Addr().String(), "externalURL", s.opts.ExternalURL, "tls", s.srv.TLSConfig != nil)
	s.logTrust()
	s.logFQDN()
	if metricsLn != nil {
		msrv := &http.Server{Handler: metrics.Handler(s.opts.Pprof), ReadHeaderTimeout: readHeaderTimeout,
			ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn)}
		s.log.Info("metrics listening", "addr", metricsLn.Addr().String(), "pprof", s.opts.Pprof)
		go func() {
			if err := msrv.Serve(metricsLn); !errors.Is(err, http.ErrServerClosed) {
				s.log.Error("metrics listener stopped", "error", err.Error())
			}
		}()
		defer func() { _ = msrv.Close() }() // a scrape needs no drain
	}

	served := make(chan error, 1)
	go func() {
		if s.srv.TLSConfig != nil {
			served <- s.srv.ServeTLS(ln, "", "") // the certificate is in TLSConfig
			return
		}
		served <- s.srv.Serve(ln)
	}()
	select {
	case err := <-served:
		return fmt.Errorf("server: %w", err)
	case <-ctx.Done():
	}
	return s.shutdown(ctx, served)
}

// shutdown is Run's drain once its context ended; served delivers the error of Serve.
func (s *Server) shutdown(ctx context.Context, served <-chan error) error {
	// Keep-alives off first: a request that sees draining writes its answer after this, so the
	// 503 of /-/readyz carries Connection: close too.
	s.srv.SetKeepAlivesEnabled(false)
	s.draining.Store(true)
	if d := s.opts.ShutdownDelay; d > 0 {
		s.log.Info("shutting down: not ready, serving until the delay ends", "delay", d, "inflight", s.inflight.Load())
		select {
		case err := <-served:
			return fmt.Errorf("server: %w", err)
		case <-time.After(d):
		}
	}
	s.log.Info("shutting down: not ready, draining in-flight requests",
		"timeout", s.opts.ShutdownTimeout, "inflight", s.inflight.Load())
	// Not http.Server.RegisterOnShutdown: it runs the hook in a goroutine of its own, so Run
	// could return before the hook did.
	if s.opts.OnShutdown != nil {
		s.opts.OnShutdown()
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(sctx); err != nil {
		s.log.Warn("shutdown timeout: aborting in-flight requests", "inflight", s.inflight.Load())
		s.abort(errAborted) // before Close, whose closed connections cancel without a cause
		_ = s.srv.Close()
		if !s.waitHandlers(abortGrace) {
			s.log.Error("handlers still running after the abort", "inflight", s.inflight.Load())
		}
		return fmt.Errorf("server: shutdown: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	s.log.Info("stopped")
	return nil
}

// track counts the running handlers for the shutdown.
func (s *Server) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.inflight.Add(1)
		defer s.inflight.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// waitHandlers waits up to d for every handler to return, and reports whether they did.
func (s *Server) waitHandlers(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for s.inflight.Load() > 0 {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}
