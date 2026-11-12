package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// recovery turns a panic in a handler into a 500 JSON response (writeInternalError), and logs
// the panic value and the stack at Error. It is the outermost middleware, so the request id
// comes from the response header that the request id middleware set.
//
// http.ErrAbortHandler is re-panicked: it is how a handler asks net/http to abort the response.
// An error that wraps it says why (format.ServeBlob's blob whose bytes are not its digest): it
// is logged at Error, in place of the access line that an aborted request does not get, and
// net/http gets http.ErrAbortHandler itself, which it does not log. When the response has
// already started, the status cannot change any more; the connection is aborted the same way,
// so the client sees a truncated response rather than a complete one.
func (s *Server) recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				if v != any(http.ErrAbortHandler) {
					s.log.ErrorContext(r.Context(), "response aborted", "error", err.Error(),
						"requestId", w.Header().Get(requestIDHeader), "method", r.Method,
						"path", auth.RedactSecrets(r.URL.Path), "status", rec.statusCode(), "bytes", rec.bytes)
				}
				panic(http.ErrAbortHandler)
			}
			id := w.Header().Get(requestIDHeader)
			s.log.ErrorContext(r.Context(), "panic",
				"panic", fmt.Sprint(v),
				"requestId", id,
				"method", r.Method,
				"path", auth.RedactSecrets(r.URL.Path),
				"stack", string(debug.Stack()),
			)
			if rec.started() {
				panic(http.ErrAbortHandler)
			}
			writeInternalError(w, r, id)
		}()
		next.ServeHTTP(rec, r)
	})
}

// writeInternalError answers 500: under /api/ with the envelope of the management API,
// {"error":{"code":"INTERNAL","message":"Internal Server Error"}}, as internal/api answers a 500
// (docs/api.md, Errors); elsewhere with {"error":"internal server error","requestId":id}.
// Headers the handler set before it failed are dropped, except the request id; the security
// headers are set again.
func writeInternalError(w http.ResponseWriter, r *http.Request, id string) {
	h := w.Header()
	for k := range h {
		if k != requestIDHeader {
			delete(h, k)
		}
	}
	setSecurityHeaders(h)
	var v any = struct {
		Error     string `json:"error"`
		RequestID string `json:"requestId"`
	}{"internal server error", id}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		v = map[string]any{"error": map[string]string{"code": "INTERNAL", "message": http.StatusText(http.StatusInternalServerError)}}
	}
	body, _ := json.Marshal(v) // strings always encode
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(append(body, '\n'))
}
