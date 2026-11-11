package server

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
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
// is not counted. Zero turns it off. A body cut off is logged at info: the client's doing,
// which the handler answers 408 (format.StatusOf, #576).
func (s *Server) stall(next http.Handler) http.Handler {
	d := s.opts.StallTimeout
	if d <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		var body *stallBody
		if r.Body != nil && r.Body != http.NoBody {
			body = &stallBody{ReadCloser: r.Body, rc: rc, d: d, cut: func(read int64) {
				s.log.InfoContext(r.Context(), "request body stalled: nothing came within server.stallTimeout, the connection is closed",
					"method", r.Method, "path", auth.RedactSecrets(r.URL.Path), "read", read, "stallTimeout", d.String(),
					"requestId", RequestID(r.Context()))
			}}
			r.Body = body
		}
		sw := &stallWriter{ResponseWriter: w, rc: rc, d: d}
		defer sw.disarm()
		next.ServeHTTP(sw, r)
		if body != nil && !body.ended {
			// net/http reads what is left of a body the handler did not finish, up to 256 KiB,
			// before it answers, to keep the connection; that read is bounded too.
			_ = rc.SetReadDeadline(time.Now().Add(d))
		}
	})
}

// stallBody sets the read deadline of the connection before each read of a request body, and
// clears it at the end of the body: net/http then reads on in the background to notice a
// client that goes away, and a deadline left behind would cancel the request while the
// handler still works. A read that misses the deadline leaves it in place, so that net/http,
// which reads the rest of a small body before it answers, fails at once and closes the
// connection instead of waiting for the client without a bound.
type stallBody struct {
	io.ReadCloser
	rc      *http.ResponseController
	d       time.Duration
	cut     func(read int64) // called once, when a read misses the deadline, with the bytes read
	read    int64
	ended   bool // a read returned an error, io.EOF included
	stalled bool // a read missed the deadline
}

func (b *stallBody) Read(p []byte) (int, error) {
	if b.stalled {
		return 0, os.ErrDeadlineExceeded
	}
	_ = b.rc.SetReadDeadline(time.Now().Add(b.d)) // ErrNotSupported: no deadline, as before
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		b.ended, b.stalled = true, true
		b.cut(b.read)
	case err != nil:
		b.ended = true
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

func (b *stallBody) Close() error {
	if !b.stalled {
		_ = b.rc.SetReadDeadline(time.Time{})
	}
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
