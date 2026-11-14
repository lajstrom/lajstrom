package generic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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
	req := func(op string, err error) error { return &url.Error{Op: op, URL: backend.String(), Err: err} }
	noAnswer := func(err error) error { return &noAnswerError{err: err} }

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
		{name: "a body written after the reset closed the connection",
			err: &url.Error{Op: "Patch", URL: backend.String(), Err: &net.OpError{Op: "readfrom", Net: "tcp",
				Err: &net.OpError{Op: "write", Net: "tcp", Err: net.ErrClosed}}},
			want: provider.ErrUnavailable},
		{name: "context canceled", err: context.Canceled},
		{name: "plain error", err: errors.New("connection reset by peer")},
		// #670: after the retries, a request that breakerTransport counted as the registry's
		// failure (noAnswer) and that timed out or met an EOF.
		{name: "dial timeout", err: req("Head", noAnswer(&net.OpError{Op: "dial", Net: "tcp", Err: transportTimeout("i/o timeout")})),
			want: provider.ErrUnavailable},
		{name: "connect timed out", err: req("Head", noAnswer(&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ETIMEDOUT)})),
			want: provider.ErrUnavailable},
		{name: "timeout awaiting response headers", err: req("Get", noAnswer(transportTimeout("net/http: timeout awaiting response headers"))),
			want: provider.ErrUnavailable},
		{name: "TLS handshake timeout", err: req("Get", noAnswer(transportTimeout("net/http: TLS handshake timeout"))),
			want: provider.ErrUnavailable},
		{name: "closed without an answer", err: req("Get", noAnswer(io.EOF)), want: provider.ErrUnavailable},
		{name: "an answer cut in its head", err: req("Get", noAnswer(io.ErrUnexpectedEOF)), want: provider.ErrUnavailable},
		{name: "an upload closed without an answer", err: fmt.Errorf("Patch: %w", noAnswer(io.EOF)), want: provider.ErrUnavailable},
		{name: "no route to host", err: req("Head", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}),
			want: provider.ErrUnavailable},
		{name: "network unreachable", err: req("Head", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}),
			want: provider.ErrUnavailable},
		// A timeout or an EOF that breakerTransport did not count as the registry's: the
		// caller's context ended, the client's body failed (#643), or no request failed.
		{name: "the caller's deadline", err: req("Get", context.DeadlineExceeded)},
		{name: "the caller's deadline while dialing", err: req("Head", &net.OpError{Op: "dial", Net: "tcp", Err: transportTimeout("i/o timeout")})},
		{name: "a deadline outside a request", err: fmt.Errorf("read back: %w", context.DeadlineExceeded)},
		{name: "a client body that broke off", err: fmt.Errorf("Patch: %w", io.ErrUnexpectedEOF)},
		{name: "a client body that stalled", err: fmt.Errorf("Patch: %w", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded})},
		{name: "an answer with an empty body", err: fmt.Errorf("GET %q: failed to decode response: %w", backend, io.EOF)},
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

// TestTranslateLimits: a registry's refusal for a limit or a quota of its own is
// provider.ErrLimit, not ErrUnauthorized (#357): a 403 or DENIED that speaks of a quota or of
// exceeding a limit, on any request; a 403 to a PATCH or a closing PUT of an upload session,
// but one that speaks of the credentials. Every other 401 and 403 stays ErrUnauthorized, and an
// immutable tag ErrConflict.
func TestTranslateLimits(t *testing.T) {
	const (
		session  = "http://harbor.internal/v2/proj/raw/r/f/blobs/uploads/5f1c?_state=abc"
		ghcr     = "http://ghcr.internal/v2/org/raw/r/f/blobs/upload/12.5f1c" // GHCR's singular upload
		manifest = "http://harbor.internal/v2/proj/raw/r/f/manifests/1.0"
		uploads  = "http://harbor.internal/v2/proj/raw/r/f/blobs/uploads/"
		// a session of a repository whose path has a manifests component, as a raw path may
		nested = "http://harbor.internal/v2/proj/raw/r/manifests/f/blobs/uploads/5f1c"
		// Harbor's quota answer to a blob PUT and a manifest PUT (docs/registries/harbor.md, Quotas)
		harborQuota = "adding 768.0 KiB of storage resource, which when updated to current usage of 512.4 KiB will exceed the configured upper limit of 1.0 MiB."
	)
	resp := func(method, rawURL string, status int, code, msg string) *errcode.ErrorResponse {
		u, _ := url.Parse(rawURL)
		e := &errcode.ErrorResponse{Method: method, URL: u, StatusCode: status}
		if code != "" {
			e.Errors = errcode.Errors{{Code: code, Message: msg}}
		}
		return e
	}
	quota, sized := "its quota", "its size limit or its quota"
	tests := []struct {
		name  string
		err   *errcode.ErrorResponse
		want  error
		limit string // the limit the message names
	}{
		{"Harbor's quota on a blob PUT", resp(http.MethodPut, session, http.StatusForbidden, errcode.ErrorCodeDenied, harborQuota), provider.ErrLimit, quota},
		{"Harbor's quota on a manifest PUT", resp(http.MethodPut, manifest, http.StatusForbidden, errcode.ErrorCodeDenied, harborQuota), provider.ErrLimit, quota},
		{"Quay's quota", resp(http.MethodPut, manifest, http.StatusForbidden, errcode.ErrorCodeDenied, "Quota has been exceeded on namespace"), provider.ErrLimit, quota},
		{"a limit exceeded on a POST", resp(http.MethodPost, uploads, http.StatusForbidden, errcode.ErrorCodeDenied, "storage limit exceeded"), provider.ErrLimit, quota},
		{"Gitea's empty 403 to the closing PUT", resp(http.MethodPut, session, http.StatusForbidden, "", ""), provider.ErrLimit, sized},
		{"a 403 to a PATCH", resp(http.MethodPatch, session, http.StatusForbidden, "", ""), provider.ErrLimit, sized},
		{"a 403 to GHCR's session", resp(http.MethodPut, ghcr, http.StatusForbidden, errcode.ErrorCodeDenied, "denied"), provider.ErrLimit, sized},
		{"a 403 to a session below a manifests component", resp(http.MethodPut, nested, http.StatusForbidden, "", ""), provider.ErrLimit, sized},
		{"a 403 to the POST", resp(http.MethodPost, uploads, http.StatusForbidden, errcode.ErrorCodeDenied, "requested access to the resource is denied"), provider.ErrUnauthorized, ""},
		{"a 403 to a manifest PUT", resp(http.MethodPut, manifest, http.StatusForbidden, "", ""), provider.ErrUnauthorized, ""},
		{"an expired token in a session", resp(http.MethodPatch, session, http.StatusForbidden, errcode.ErrorCodeDenied, "Your authorization token has expired. Reauthenticate and try again."), provider.ErrUnauthorized, ""},
		{"UNAUTHORIZED in a session", resp(http.MethodPut, session, http.StatusForbidden, errcode.ErrorCodeUnauthorized, "access to the requested resource is not authorized"), provider.ErrUnauthorized, ""},
		{"a 401 in a session", resp(http.MethodPut, session, http.StatusUnauthorized, "", ""), provider.ErrUnauthorized, ""},
		{"a 401 that speaks of a quota", resp(http.MethodPut, manifest, http.StatusUnauthorized, errcode.ErrorCodeUnauthorized, "quota"), provider.ErrUnauthorized, ""},
		{"an immutable tag", resp(http.MethodPut, manifest, http.StatusForbidden, errcode.ErrorCodeDenied, "the tag 1.0 is immutable"), provider.ErrConflict, ""},
		{"a 413 stays a refusal", resp(http.MethodPut, session, http.StatusRequestEntityTooLarge, "", ""), provider.ErrRejected, ""},
		{"a 416 that exceeds a limit stays a refusal", resp(http.MethodPatch, ghcr, http.StatusRequestedRangeNotSatisfiable, "RANGE_INVALID",
			"the request body is too large and exceeds the maximum permissible limit of 4.00MiB"), provider.ErrRejected, ""},
	}
	sentinels := []error{provider.ErrLimit, provider.ErrUnauthorized, provider.ErrConflict, provider.ErrRejected}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translateAs("harbor", noWait, "finish upload raw/r/f", tt.err)
			for _, s := range sentinels {
				if is := errors.Is(got, s); is != errors.Is(s, tt.want) {
					t.Errorf("errors.Is(%v, %v) = %v", got, s, is)
				}
			}
			checkCause(t, got, tt.err)
			msg := got.Error()
			if strings.Contains(msg, "harbor.internal") || strings.Contains(msg, "ghcr.internal") || strings.Contains(msg, "proj/") ||
				strings.Contains(msg, "_state") || strings.Contains(msg, "768.0 KiB") {
				t.Errorf("message %q shows the URL, the instance prefix or the registry's own words", msg)
			}
			if tt.limit != "" && !strings.HasSuffix(msg, ": "+tt.limit+")") || tt.limit != "" && !strings.Contains(msg, "registry harbor") {
				t.Errorf("message %q does not name the registry and %q", msg, tt.limit)
			}
		})
	}
	t.Run("message", func(t *testing.T) {
		got := translateAs("gitea", noWait, "finish upload raw/r/f", resp(http.MethodPut, session, http.StatusForbidden, "", "")).Error()
		want := "generic: finish upload raw/r/f: provider: refused for a limit or a quota of the registry " +
			"(registry gitea, an upload it had opened, HTTP 403: its size limit or its quota)"
		if got != want {
			t.Errorf("message = %q, want %q", got, want)
		}
	})
}

// TestLimitLogged: a write that the registry refuses for a limit, through the provider, is
// provider.ErrLimit and a warning that names the registry, for the operator: the client's 403
// is not logged elsewhere (#357). Gitea's empty 403 to the closing PUT of a blob, Harbor's
// quota on a manifest PUT.
func TestLimitLogged(t *testing.T) {
	f := newFakeRegistry(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/"):
			w.WriteHeader(http.StatusForbidden)
			return true
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/"):
			writeErr(w, http.StatusForbidden, errcode.ErrorCodeDenied, "adding 1.0 KiB of storage resource, which when updated to "+
				"current usage of 1.0 MiB will exceed the configured upper limit of 1.0 MiB.")
			return true
		}
		return false
	}
	log, buf := testLogger()
	p := f.provider(t, log, nil)
	ctx := context.Background()
	rr, err := p.Repository(ctx, "raw/r/f")
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte("over the limit")
	desc := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(blob), Size: int64(len(blob))}
	manifest := []byte(`{"schemaVersion":2}`)
	mdesc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest))}
	for name, push := range map[string]func() error{
		"blob":     func() error { return rr.Push(ctx, desc, bytes.NewReader(blob)) },
		"manifest": func() error { return rr.PushReference(ctx, mdesc, bytes.NewReader(manifest), "1.0") },
	} {
		err := push()
		if !errors.Is(err, provider.ErrLimit) || errors.Is(err, provider.ErrUnauthorized) {
			t.Errorf("%s: %v, want ErrLimit", name, err)
		} else if strings.Contains(err.Error(), "art/") || strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("%s: message %q shows the instance prefix or the URL", name, err)
		}
	}
	if got := strings.Count(buf.String(), "registry refused a write for a limit or a quota of its own"); got != 2 {
		t.Errorf("%d warnings, want 2; log:\n%s", got, buf)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "registry=fake") {
		t.Errorf("the warnings are not at warn or do not name the registry:\n%s", buf)
	}
}

// noWait is the wait of a registry that asked for none.
func noWait() time.Duration { return 0 }

// transportTimeout is a timeout of the transport as net and net/http make one: a dial past
// Dialer.Timeout ("i/o timeout"), a wait for the response headers past ResponseHeaderTimeout,
// a TLS handshake past TLSHandshakeTimeout. Like theirs, errors.Is matches
// context.DeadlineExceeded, so that a caller's deadline cannot be told from it by the error.
type transportTimeout string

func (e transportTimeout) Error() string { return string(e) }

func (transportTimeout) Timeout() bool { return true }

func (transportTimeout) Temporary() bool { return true }

func (transportTimeout) Is(target error) bool { return target == context.DeadlineExceeded }

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
