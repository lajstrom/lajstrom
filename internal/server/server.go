package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// DefaultShutdownTimeout is the graceful shutdown bound when Options.ShutdownTimeout is zero.
const DefaultShutdownTimeout = 30 * time.Second

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
	// ShutdownTimeout bounds the graceful shutdown; zero means DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
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
}

// Server is the Lajstrom HTTP server. Create it with New and start it with Run.
type Server struct {
	opts    Options
	log     *slog.Logger
	mux     *http.ServeMux
	handler http.Handler
	srv     *http.Server

	running     atomic.Bool
	bound       chan struct{} // closed when Run has bound the listeners, or failed to
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
	if o.Log == nil {
		o.Log = slog.Default()
	}
	s := &Server{opts: o, log: o.Log, mux: http.NewServeMux(), bound: make(chan struct{})}
	s.routes()
	s.handler = s.recovery(requestID(s.accessLog(s.authenticate(s.mux))))
	s.srv = &http.Server{
		Handler: s.handler,
		// No ReadTimeout and no WriteTimeout: they bound the whole request body and the whole
		// response, and a blob upload or download streams for as long as its size needs. Slow
		// clients are bounded by ReadHeaderTimeout and IdleTimeout; per-route limits are #50.
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(o.Log.Handler(), slog.LevelWarn),
	}
	return s, nil
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

// Run listens on Options.Listen and serves until ctx is cancelled. Then it stops accepting
// connections and waits up to ShutdownTimeout for in-flight requests to finish. It returns nil
// after a clean stop, and an error when it cannot listen, when serving fails, or when requests
// are still running at the deadline (their connections are closed). Run may be called once.
func (s *Server) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("server: Run called twice")
	}
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
	s.log.Info("listening", "addr", ln.Addr().String(), "externalURL", s.opts.ExternalURL)
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
	go func() { served <- s.srv.Serve(ln) }()
	select {
	case err := <-served:
		return fmt.Errorf("server: %w", err)
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "timeout", s.opts.ShutdownTimeout)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(sctx); err != nil {
		_ = s.srv.Close()
		return fmt.Errorf("server: shutdown: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	s.log.Info("stopped")
	return nil
}
