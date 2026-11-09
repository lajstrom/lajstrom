package server

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// uploadRetryAfter is the Retry-After, in seconds, of an upload refused because
// Options.MaxUploads are running.
const uploadRetryAfter = "5"

// stall bounds how long the body of a request, and the response, may make no progress: each
// read of the body and each write of the response must move within Options.StallTimeout, or
// the connection is closed. The deadline moves with every read and write, so a transfer of
// any size and length passes while it moves; a client that stops sending or stops reading
// does not hold a connection, a goroutine, an upload slot and a connection to the registry
// for ever. The time the handler spends between two reads or writes (a registry, the index)
// is not counted. Zero turns it off.
func (s *Server) stall(next http.Handler) http.Handler {
	d := s.opts.StallTimeout
	if d <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &stallBody{ReadCloser: r.Body, rc: rc, d: d}
		}
		sw := &stallWriter{ResponseWriter: w, rc: rc, d: d}
		defer sw.disarm()
		next.ServeHTTP(sw, r)
	})
}

// stallBody sets the read deadline of the connection before each read of a request body, and
// clears it at the end of the body: net/http then reads on in the background to notice a
// client that goes away, and a deadline left behind would cancel the request while the
// handler still works.
type stallBody struct {
	io.ReadCloser
	rc *http.ResponseController
	d  time.Duration
}

func (b *stallBody) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(b.d)) // ErrNotSupported: no deadline, as before
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

func (b *stallBody) Close() error {
	_ = b.rc.SetReadDeadline(time.Time{})
	return b.ReadCloser.Close()
}

// stallWriter sets the write deadline of the connection before each write and flush of the
// response, and clears it when the handler returns: the server does not reset it for the next
// request on the connection.
type stallWriter struct {
	http.ResponseWriter
	rc    *http.ResponseController
	d     time.Duration
	armed bool
}

func (w *stallWriter) arm() {
	w.armed = w.rc.SetWriteDeadline(time.Now().Add(w.d)) == nil
}

func (w *stallWriter) disarm() {
	if w.armed {
		_ = w.rc.SetWriteDeadline(time.Time{})
	}
}

func (w *stallWriter) Write(p []byte) (int, error) {
	w.arm()
	return w.ResponseWriter.Write(p)
}

// Flush sends buffered data within the deadline, when the underlying writer can.
func (w *stallWriter) Flush() {
	w.arm()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying writer for http.ResponseController.
func (w *stallWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// limitUploads lets at most Options.MaxUploads uploads run at once and answers the
// next one 503 with Retry-After, before its body is read: an upload holds a connection to the
// registry and, for a frontend that spools, a temporary file for as long as it streams. Zero
// means no limit.
func (s *Server) limitUploads(next http.Handler) http.Handler {
	n := s.opts.MaxUploads
	if n <= 0 {
		return next
	}
	slots := make(chan struct{}, n)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isUpload(r) {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			next.ServeHTTP(w, r)
		default:
			tooManyUploads(w, r)
		}
	})
}

// isUpload reports whether r is an upload: a POST, PUT or PATCH to a frontend or to the OCI
// facade, not to the management API, the system endpoints or the token endpoint.
func isUpload(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return false
	}
	p := r.URL.Path
	return !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/-/") && p != "/v2/token"
}

// tooManyUploads answers an upload over the limit: 503 with Retry-After, and on /v2 the error
// body of the distribution API with UNAVAILABLE, which docker retries.
func tooManyUploads(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", uploadRetryAfter)
	if r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/") {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"errors":[{"code":"UNAVAILABLE","message":"too many uploads at once; retry later"}]}`+"\n")
		return
	}
	writeText(w, http.StatusServiceUnavailable, "too many uploads at once; retry later\n")
}
