package generic

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/lajstrom/lajstrom/internal/provider"
)

func TestTranslate(t *testing.T) {
	backend, _ := url.Parse("http://zot.internal:5000/v2/art/raw/scratch/notes/manifests/v1")
	resp := func(status int, codes ...string) *errcode.ErrorResponse {
		e := &errcode.ErrorResponse{Method: http.MethodGet, URL: backend, StatusCode: status}
		for _, c := range codes {
			e.Errors = append(e.Errors, errcode.Error{Code: c, Message: strings.ToLower(c) + " for art/raw/scratch/notes"})
		}
		return e
	}
	immutable := resp(http.StatusBadRequest, errcode.ErrorCodeDenied)
	immutable.Errors[0].Message = "The tag v1 is Immutable and cannot be overwritten"
	sentinels := []error{provider.ErrNotFound, provider.ErrConflict, provider.ErrUnsupported, provider.ErrUnauthorized, provider.ErrUnavailable}
	unresolved := func(dns *net.DNSError) error {
		return &url.Error{Op: "Head", URL: backend.String(), Err: &net.OpError{Op: "dial", Net: "tcp", Err: dns}}
	}

	tests := []struct {
		name string
		err  error
		want error // the one sentinel the result wraps; nil for none
	}{
		{name: "errdef not found", err: fmt.Errorf("zot:5000/art/x@sha256:1: %w", errdef.ErrNotFound), want: provider.ErrNotFound},
		{name: "404 without a body", err: resp(http.StatusNotFound), want: provider.ErrNotFound},
		{name: "NAME_UNKNOWN", err: resp(http.StatusNotFound, errcode.ErrorCodeNameUnknown), want: provider.ErrNotFound},
		{name: "MANIFEST_UNKNOWN", err: resp(http.StatusNotFound, errcode.ErrorCodeManifestUnknown), want: provider.ErrNotFound},
		{name: "BLOB_UNKNOWN on a 400", err: resp(http.StatusBadRequest, errcode.ErrorCodeBlobUnknown), want: provider.ErrNotFound},
		{name: "wrapped error response", err: fmt.Errorf("GET %q: %w", backend, resp(http.StatusNotFound, errcode.ErrorCodeManifestUnknown)), want: provider.ErrNotFound},
		{name: "401", err: resp(http.StatusUnauthorized, errcode.ErrorCodeUnauthorized), want: provider.ErrUnauthorized},
		{name: "403", err: resp(http.StatusForbidden), want: provider.ErrUnauthorized},
		{name: "DENIED", err: resp(http.StatusForbidden, errcode.ErrorCodeDenied), want: provider.ErrUnauthorized},
		{name: "no credentials for a Basic challenge", err: fmt.Errorf("GET %q: %w", backend, auth.ErrBasicCredentialNotFound), want: provider.ErrUnauthorized},
		{name: "405", err: resp(http.StatusMethodNotAllowed, errcode.ErrorCodeUnsupported), want: provider.ErrUnsupported},
		{name: "UNSUPPORTED on a 400", err: resp(http.StatusBadRequest, errcode.ErrorCodeUnsupported), want: provider.ErrUnsupported},
		{name: "errdef unsupported", err: fmt.Errorf("referrers: %w", errdef.ErrUnsupported), want: provider.ErrUnsupported},
		{name: "409", err: resp(http.StatusConflict), want: provider.ErrConflict},
		{name: "TAG_INVALID", err: resp(http.StatusBadRequest, "TAG_INVALID"), want: provider.ErrConflict},
		{name: "DENIED on an immutable tag", err: immutable, want: provider.ErrConflict},
		{name: "500", err: resp(http.StatusInternalServerError, "UNKNOWN")},
		{name: "400 MANIFEST_INVALID", err: resp(http.StatusBadRequest, errcode.ErrorCodeManifestInvalid)},
		{name: "no such host", err: unresolved(&net.DNSError{Err: "no such host", Name: "zot.internal", IsNotFound: true}),
			want: provider.ErrUnavailable},
		{name: "temporary failure in name resolution",
			err:  unresolved(&net.DNSError{Err: "Temporary failure in name resolution", Name: "zot.internal", IsTemporary: true}),
			want: provider.ErrUnavailable},
		{name: "connection refused", err: &url.Error{Op: "Head", URL: backend.String(), Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}},
			want: provider.ErrUnavailable},
		{name: "connection reset",
			err: &url.Error{Op: "Get", URL: backend.String(), Err: fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w",
				&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)})},
			want: provider.ErrUnavailable},
		{name: "broken pipe", err: &url.Error{Op: "Put", URL: backend.String(), Err: &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}},
			want: provider.ErrUnavailable},
		{name: "context canceled", err: context.Canceled},
		{name: "plain error", err: errors.New("connection reset by peer")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translateAs("fake", noWait, "fetch raw/scratch/notes@v1", tt.err)
			if got == nil {
				t.Fatal("translate returned nil")
			}
			for _, s := range sentinels {
				if is, want := errors.Is(got, s), errors.Is(s, tt.want); is != want {
					t.Errorf("errors.Is(%v, %v) = %v, want %v", got, s, is, want)
				}
			}
			checkCause(t, got, tt.err)
			if !strings.HasPrefix(got.Error(), "generic: fetch raw/scratch/notes@v1: ") {
				t.Errorf("message %q does not name the operation", got)
			}
			if tt.want != nil && (strings.Contains(got.Error(), "zot.internal") || strings.Contains(got.Error(), "art/")) {
				t.Errorf("message %q shows the backend URL or the instance prefix", got)
			}
		})
	}

	t.Run("nil", func(t *testing.T) {
		if err := translateAs("fake", noWait, "fetch", nil); err != nil {
			t.Errorf("translate(nil) = %v", err)
		}
	})
	t.Run("detail", func(t *testing.T) {
		got := translateAs("fake", noWait, "tags raw/x", resp(http.StatusNotFound, errcode.ErrorCodeNameUnknown)).Error()
		if want := "generic: tags raw/x: provider: not found (HTTP 404, NAME_UNKNOWN)"; got != want {
			t.Errorf("message = %q, want %q", got, want)
		}
	})
}

// TestTranslateRefusals: a status no sentinel names is the registry's refusal when it is a 4xx
// or a 503 (#494), busy for a 429 or 503 with the wait the registry asked for; any other 5xx is
// not.
func TestTranslateRefusals(t *testing.T) {
	backend, _ := url.Parse("http://zot.internal:5000/v2/art/raw/x/blobs/uploads/")
	resp := func(status int, codes ...string) *errcode.ErrorResponse {
		e := &errcode.ErrorResponse{Method: http.MethodPost, URL: backend, StatusCode: status}
		for _, c := range codes {
			e.Errors = append(e.Errors, errcode.Error{Code: c, Message: strings.ToLower(c) + " for art/raw/x"})
		}
		return e
	}
	for _, tt := range []struct {
		err    *errcode.ErrorResponse
		code   string
		busy   bool
		reject bool
	}{
		{err: resp(http.StatusBadRequest, errcode.ErrorCodeManifestInvalid), code: errcode.ErrorCodeManifestInvalid, reject: true},
		{err: resp(http.StatusRequestEntityTooLarge), reject: true},
		{err: resp(http.StatusTooManyRequests, "TOOMANYREQUESTS"), code: "TOOMANYREQUESTS", busy: true, reject: true},
		{err: resp(http.StatusServiceUnavailable), busy: true, reject: true},
		{err: resp(http.StatusBadGateway)},
	} {
		t.Run(fmt.Sprintf("refusal %d", tt.err.StatusCode), func(t *testing.T) {
			got := translateAs("fake", func() time.Duration { return 7 * time.Second }, "push raw/x", tt.err)
			var rej *provider.RejectedError
			if errors.As(got, &rej) != tt.reject || errors.Is(got, provider.ErrUnavailable) != tt.busy {
				t.Fatalf("%v: a refusal %v, busy %v; want %v, %v", got, rej != nil, errors.Is(got, provider.ErrUnavailable), tt.reject, tt.busy)
			}
			checkCause(t, got, tt.err)
			if !tt.reject {
				return
			}
			if rej.Registry != "fake" || rej.Status != tt.err.StatusCode || rej.Code != tt.code ||
				rej.RetryAfter != map[bool]time.Duration{true: 7 * time.Second}[tt.busy] {
				t.Errorf("refusal = %+v", rej)
			}
			if strings.Contains(got.Error(), "zot.internal") || strings.Contains(got.Error(), "art/") {
				t.Errorf("message %q shows the backend URL or the instance prefix", got)
			}
		})
	}
}

// noWait is the wait of a registry that asked for none.
func noWait() time.Duration { return 0 }

// checkCause fails t when the translated error got lost its cause: errors.Is must find the
// cause and errors.As the *errcode.ErrorResponse in it.
func checkCause(t *testing.T, got, cause error) {
	t.Helper()
	if !errors.Is(got, cause) {
		t.Errorf("the cause is lost: errors.Is(result, cause) is false")
	}
	var er *errcode.ErrorResponse
	if !errors.As(cause, &er) {
		return
	}
	var back *errcode.ErrorResponse
	if !errors.As(got, &back) || back != er {
		t.Errorf("errors.As does not find the *errcode.ErrorResponse")
	}
}
