package format_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

func TestWriteError(t *testing.T) {
	tooLarge := &http.MaxBytesError{Limit: 1024}
	tests := []struct {
		name       string
		err        error
		status     int
		retryAfter string // the Retry-After header; empty for none
	}{
		{name: "invalid", err: format.ErrInvalid, status: 400},
		{name: "wrapped invalid", err: fmt.Errorf("pypi: wheel: %w", format.ErrInvalid), status: 400},
		{name: "unauthenticated", err: fmt.Errorf("%w: read on fake/scratch", format.ErrUnauthenticated), status: 401},
		{name: "forbidden", err: fmt.Errorf("%w: write on fake/scratch", format.ErrForbidden), status: 403},
		{name: "index not found", err: fmt.Errorf("raw: %w", index.ErrNotFound), status: 404},
		{name: "provider not found", err: fmt.Errorf("raw: blob: %w", provider.ErrNotFound), status: 404},
		{name: "index conflict", err: index.ErrConflict, status: 409},
		{name: "tag conflict", err: fmt.Errorf("ingest: %w", &index.TagConflictError{Tag: "1.0", Existing: "sha256:00"}), status: 409},
		{name: "provider conflict", err: provider.ErrConflict, status: 409},
		{name: "body too large", err: tooLarge, status: 413},
		{name: "wrapped body too large", err: fmt.Errorf("raw: upload: %w", tooLarge), status: 413},
		{name: "unsupported", err: format.ErrUnsupported, status: 501},
		{name: "provider unsupported", err: fmt.Errorf("ecr: %w", provider.ErrUnsupported), status: 501},
		{name: "backend rejected our credentials", err: fmt.Errorf("generic: %w", provider.ErrUnauthorized), status: 502},
		{name: "circuit breaker open", err: fmt.Errorf("raw: push: %w", &provider.UnavailableError{RetryAfter: 12 * time.Second}), status: 503, retryAfter: "12"},
		{name: "unavailable without a time", err: provider.ErrUnavailable, status: 503},
		{name: "first match wins", err: errors.Join(index.ErrNotFound, format.ErrInvalid), status: 400},
		{name: "other", err: errors.New("index: database is unreachable"), status: 500},
		{name: "nil", status: 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := format.StatusOf(tt.err); got != tt.status {
				t.Errorf("StatusOf = %d, want %d", got, tt.status)
			}
			rr := httptest.NewRecorder()
			rr.Header().Set("Content-Length", "9999") // set by a handler before it failed
			if got := format.WriteError(rr, tt.err); got != tt.status {
				t.Errorf("WriteError = %d, want %d", got, tt.status)
			}
			body := http.StatusText(tt.status) + "\n"
			if tt.status < 500 {
				body = tt.err.Error() + "\n"
			}
			challenge := ""
			if tt.status == http.StatusUnauthorized {
				challenge = `Basic realm="lajstrom"`
			}
			h := rr.Header()
			switch {
			case rr.Code != tt.status:
				t.Errorf("status = %d, want %d", rr.Code, tt.status)
			case rr.Body.String() != body:
				t.Errorf("body = %q, want %q", rr.Body, body)
			case h.Get("Content-Type") != "text/plain; charset=utf-8" || h.Get("X-Content-Type-Options") != "nosniff":
				t.Errorf("Content-Type %q, X-Content-Type-Options %q", h.Get("Content-Type"), h.Get("X-Content-Type-Options"))
			case h.Get("WWW-Authenticate") != challenge:
				t.Errorf("WWW-Authenticate = %q, want %q", h.Get("WWW-Authenticate"), challenge)
			case h.Get("Content-Length") != "":
				t.Errorf("stale Content-Length %q", h.Get("Content-Length"))
			case h.Get("Retry-After") != tt.retryAfter:
				t.Errorf("Retry-After = %q, want %q", h.Get("Retry-After"), tt.retryAfter)
			}
		})
	}
}

// TestSetRetryAfter checks the seconds of Retry-After: the time left rounded up, at least 1, and
// no header for an error that does not say when to try again.
func TestSetRetryAfter(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{&provider.UnavailableError{RetryAfter: 0}, "1"},
		{&provider.UnavailableError{RetryAfter: 300 * time.Millisecond}, "1"},
		{&provider.UnavailableError{RetryAfter: time.Second}, "1"},
		{&provider.UnavailableError{RetryAfter: 1200 * time.Millisecond}, "2"},
		{fmt.Errorf("facade: %w", &provider.UnavailableError{RetryAfter: 30 * time.Second}), "30"},
		{provider.ErrUnavailable, ""},
		{errors.New("other"), ""},
		{nil, ""},
	}
	for _, tt := range tests {
		h := http.Header{}
		format.SetRetryAfter(h, tt.err)
		if got := h.Get("Retry-After"); got != tt.want {
			t.Errorf("SetRetryAfter(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}
