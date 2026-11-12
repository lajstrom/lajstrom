package server

import (
	"net/http"
	"strconv"
)

// recorder is an http.ResponseWriter that remembers the status code and counts the body bytes
// for the middleware, with what it needs to tell a response cut short (short). It keeps
// http.Flusher working, so streamed responses are not buffered, and unwraps for
// http.ResponseController (deadlines, hijacking).
type recorder struct {
	http.ResponseWriter
	status   int   // final status code; 0 until the header is written
	bytes    int64 // body bytes written
	declared int64 // the Content-Length of the header, 0 without one
	failed   bool  // a write of the body failed
}

// WriteHeader records the first final status code; 1xx informational headers are not final.
func (r *recorder) WriteHeader(code int) {
	if r.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		r.sent(code)
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write writes body bytes; the first write sends the header with 200 unless one was written.
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.sent(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	r.failed = r.failed || err != nil
	return n, err
}

// Flush sends buffered data to the client, when the underlying writer can.
func (r *recorder) Flush() {
	if r.status == 0 {
		r.sent(http.StatusOK)
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying writer for http.ResponseController.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// sent records the final status of the header being sent, and the length it declares.
func (r *recorder) sent(code int) {
	r.status = code
	r.declared, _ = strconv.ParseInt(r.Header().Get("Content-Length"), 10, 64) // 0 without one
}

// short reports whether the body the client got is shorter than the handler meant: fewer bytes
// than the header declared, or a write that failed. A response without a body (HEAD, 204, 304)
// is never short.
func (r *recorder) short(method string) bool {
	if method == http.MethodHead || r.status == http.StatusNoContent || r.status == http.StatusNotModified {
		return false
	}
	return r.failed || r.bytes < r.declared
}

// started reports whether the header has been sent.
func (r *recorder) started() bool { return r.status != 0 }

// statusCode is the status the client received: 200 when the handler wrote nothing.
func (r *recorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
