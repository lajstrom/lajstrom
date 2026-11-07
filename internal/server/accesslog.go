package server

import (
	"log/slog"
	"net/http"
	"time"
)

// accessLog writes one Info line per request after the handler returns. It logs the path and
// never the query string or any header: queries and Authorization carry credentials. The
// principal is the one the authentication middleware settled on, through a holder in the
// context, anonymous when it settled on none, the requests it refused included. A request
// whose handler panics has no access line; the recovery's error line replaces it.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		ctx, held := withPrincipalHolder(r.Context())
		next.ServeHTTP(rec, r.WithContext(ctx))
		s.log.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.statusCode()),
			slog.Int64("bytes", rec.bytes),
			slog.Duration("duration", time.Since(start)),
			slog.String("requestId", RequestID(r.Context())),
			slog.String("remoteAddr", r.RemoteAddr),
			slog.String("principal", held.p.String()),
		)
	})
}
