package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// accessLog writes one Info line per request after the handler returns. It logs the path, its
// token secrets redacted (npm logout has one in it), and never the query string or any header:
// queries and Authorization carry credentials. The
// remoteAddr is the client's (clientAddress): the peer of the connection with its port, or the
// address X-Forwarded-For names, without a port, with the peer as proxy. The
// principal is the one the authentication middleware settled on, through a holder in the
// context, anonymous when it settled on none, the requests it refused included. A request
// whose handler panics has no access line; the recovery's error line replaces it. The same
// place records the HTTP metrics, by the route pattern the request matched ("unmatched" for
// none), never by its path.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		ctx, held := withPrincipalHolder(r.Context())
		next.ServeHTTP(rec, r.WithContext(ctx))
		s.observe(r, rec, time.Since(start))
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", auth.RedactSecrets(r.URL.Path)),
			slog.Int("status", rec.statusCode()),
			slog.Int64("bytes", rec.bytes),
			slog.Duration("duration", time.Since(start)),
			slog.String("requestId", RequestID(r.Context())),
			slog.String("remoteAddr", r.RemoteAddr),
			slog.String("principal", held.p.String()),
		}
		if c, _ := r.Context().Value(clientKey{}).(client); c.proxy != "" {
			attrs[6] = slog.String("remoteAddr", c.addr)
			attrs = append(attrs, slog.String("proxy", r.RemoteAddr))
		}
		s.log.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
	})
}

// observe records a request in the HTTP metrics.
func (s *Server) observe(r *http.Request, rec *recorder, took time.Duration) {
	_, route := s.mux.Handler(r)
	if route == "" {
		route = "unmatched"
	}
	labels := []string{r.Method, route, metrics.Status(rec.statusCode())}
	metrics.HTTPDuration.WithLabelValues(labels...).Observe(took.Seconds())
	metrics.HTTPResponseSize.WithLabelValues(labels...).Observe(float64(rec.bytes))
	if r.ContentLength >= 0 {
		metrics.HTTPRequestSize.WithLabelValues(labels...).Observe(float64(r.ContentLength))
	}
}
