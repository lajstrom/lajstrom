package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
)

// recovery turns a panic in a handler into a 500 JSON response with the request id, and logs
// the panic value and the stack at Error. It is the outermost middleware, so the request id
// comes from the response header that the request id middleware set.
//
// http.ErrAbortHandler is re-panicked: it is how a handler asks net/http to abort the response.
// When the response has already started, the status cannot change any more; the connection is
// aborted the same way, so the client sees a truncated response rather than a complete one.
func (s *Server) recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			id := w.Header().Get(requestIDHeader)
			s.log.ErrorContext(r.Context(), "panic",
				"panic", fmt.Sprint(v),
				"requestId", id,
				"method", r.Method,
				"path", r.URL.Path,
				"stack", string(debug.Stack()),
			)
			if rec.started() {
				panic(http.ErrAbortHandler)
			}
			writeInternalError(w, id)
		}()
		next.ServeHTTP(rec, r)
	})
}

// writeInternalError answers 500 with {"error":"internal server error","requestId":id}. Headers
// the handler set before it failed are dropped, except the request id; the security headers
// are set again.
func writeInternalError(w http.ResponseWriter, id string) {
	h := w.Header()
	for k := range h {
		if k != requestIDHeader {
			delete(h, k)
		}
	}
	setSecurityHeaders(h)
	body, _ := json.Marshal(struct {
		Error     string `json:"error"`
		RequestID string `json:"requestId"`
	}{"internal server error", id})
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(append(body, '\n'))
}
