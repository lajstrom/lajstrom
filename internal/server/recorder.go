package server

import "net/http"

// recorder is an http.ResponseWriter that remembers the status code and counts the body bytes
// for the middleware. It keeps http.Flusher working, so streamed responses are not buffered,
// and unwraps for http.ResponseController (deadlines, hijacking).
type recorder struct {
	http.ResponseWriter
	status int   // final status code; 0 until the header is written
	bytes  int64 // body bytes written
}

// WriteHeader records the first final status code; 1xx informational headers are not final.
func (r *recorder) WriteHeader(code int) {
	if r.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write writes body bytes; the first write sends the header with 200 unless one was written.
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush sends buffered data to the client, when the underlying writer can.
func (r *recorder) Flush() {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying writer for http.ResponseController.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// started reports whether the header has been sent.
func (r *recorder) started() bool { return r.status != 0 }

// statusCode is the status the client received: 200 when the handler wrote nothing.
func (r *recorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
