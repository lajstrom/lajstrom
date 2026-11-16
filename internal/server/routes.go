package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/version"
)

// routes registers the system endpoints, the management API, the OCI facade and the format
// plugins on the mux. Any other path is answered 404 by the ServeMux.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /-/healthz", s.healthz)
	s.mux.HandleFunc("GET /-/readyz", s.readyz)
	s.mux.HandleFunc("GET /-/version", s.buildInfo)
	if s.opts.API != nil {
		s.mux.Handle("/api/", s.opts.API)
	}
	if s.opts.Facade != nil {
		s.mux.Handle("/v2/", s.opts.Facade)
	}
	if s.opts.OIDC != nil {
		s.mux.Handle("/-/oidc/", s.opts.OIDC)
	}
	if s.opts.CIExchange != nil {
		s.mux.HandleFunc("POST "+oidcTokenPath, s.oidcToken)
	}
	s.mountFormats()
}

// mountFormats mounts each plugin's Routes below /<name>/ through format.NewRouter, which
// resolves {repo} once per request, and the documents of /.well-known/ of a plugin that has
// some (wellKnown). Every plugin gets Options.Deps with a logger that carries
// format=<name>: Deps.Log, or the server's when it is nil.
func (s *Server) mountFormats() {
	for _, p := range s.opts.Plugins {
		name := p.Name()
		deps := s.opts.Deps
		if deps.Log == nil {
			deps.Log = s.log
		}
		deps.Log = deps.Log.With("format", name)
		p.Routes(format.NewRouter(s.mux, name, deps), deps)
		if wk, ok := p.(wellKnown); ok {
			for doc, h := range wk.WellKnown() {
				s.mux.Handle("GET /.well-known/"+doc, h)
			}
		}
		s.log.Info("format mounted", "format", name, "artifactType", p.ArtifactType())
	}
}

// wellKnown is a plugin that serves documents of the host's /.well-known/, by name: terraform's
// terraform.json, the service discovery of the main host (docs/spec/http-routing.md §3,
// terraform). The server mounts each as GET /.well-known/<name>; the handler authorizes what it
// needs, the authentication middleware having refused credentials that do not verify.
type wellKnown interface {
	WellKnown() map[string]http.Handler
}

// healthz answers 200 while the process serves HTTP.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, "ok\n")
}

// readyz answers 503 while the server shuts down, then runs Options.Ready and answers 503
// with its error, or 200.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeText(w, http.StatusServiceUnavailable, "shutting down\n")
		return
	}
	if s.opts.Ready != nil {
		if err := s.opts.Ready(r.Context()); err != nil {
			s.log.WarnContext(r.Context(), "not ready", "error", err.Error(), "requestId", RequestID(r.Context()))
			writeText(w, http.StatusServiceUnavailable, err.Error()+"\n")
			return
		}
	}
	writeText(w, http.StatusOK, "ok\n")
}

// buildInfo answers the build information as JSON.
func (s *Server) buildInfo(w http.ResponseWriter, r *http.Request) {
	body, err := json.Marshal(version.Get())
	if err != nil {
		s.log.ErrorContext(r.Context(), "encode version", "error", err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(body, '\n'))
}

// writeText writes a plain-text response; write errors mean the client went away.
func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
