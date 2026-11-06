package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/version"
)

// routes registers the system endpoints and the format plugins on the mux. Any other path is
// answered 404 by the ServeMux.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /-/healthz", s.healthz)
	s.mux.HandleFunc("GET /-/readyz", s.readyz)
	s.mux.HandleFunc("GET /-/version", s.buildInfo)
	s.mountFormats()
}

// mountFormats is the extension point of issue #11: there each plugin's Routes is mounted below
// /<name>/ through the format.Router implementation, with the repo middleware and one
// format.Deps. Until then the plugins are only validated (New) and logged, and their paths
// answer 404.
func (s *Server) mountFormats() {
	for _, p := range s.opts.Plugins {
		s.log.Info("format registered", "format", p.Name(), "artifactType", p.ArtifactType())
	}
}

// healthz answers 200 while the process serves HTTP.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, "ok\n")
}

// readyz runs Options.Ready and answers 503 with its error, or 200.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
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
