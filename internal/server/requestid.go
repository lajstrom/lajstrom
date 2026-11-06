package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// requestIDHeader carries the request id in requests and responses.
const requestIDHeader = "X-Request-Id"

// maxRequestIDLen is the longest client-supplied request id that is kept.
const maxRequestIDLen = 64

type requestIDKey struct{}

// RequestID returns the id of the request ctx belongs to, or "" outside a request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// requestID gives every request an id: the client's X-Request-Id when it is safe to log and
// echo (validRequestID), a new random one otherwise. The id is set on the response and stored
// in the request context for RequestID.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// validRequestID reports whether id is 1 to 64 characters of [A-Za-z0-9_.-].
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// newRequestID returns 16 random bytes in hex.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read does not return errors; it crashes the program instead
	return hex.EncodeToString(b[:])
}
