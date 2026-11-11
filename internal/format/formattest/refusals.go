package formattest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// Refusal is the registry's refusal of what a frontend's upload sends it, as the provider
// reports it, and what the frontend answers its client (format.StatusOf, #494).
type Refusal struct {
	Name string
	// Push fails the blob pushes of a providertest.Fake and its blob upload sessions,
	// PushReference its manifest pushes.
	Push, PushReference error
	// Status is the frontend's answer, RetryAfter its Retry-After, "" for none.
	Status     int
	RetryAfter string
}

// Refusals are the refusals that every frontend's tests upload against: a blob over the
// registry's size stays 413; a 429 or 503, the registry throttling Lajstrom's credential, is
// 503 with its Retry-After, 1 when it sent none; any other refusal of what Lajstrom built and
// sent is 502.
var Refusals = []Refusal{
	{Name: "blob over the registry's size", Push: refusal(http.StatusRequestEntityTooLarge, "SIZE_INVALID", 0), Status: http.StatusRequestEntityTooLarge},
	{Name: "blob refused", Push: refusal(http.StatusBadRequest, "BLOB_UPLOAD_INVALID", 0), Status: http.StatusBadGateway},
	{Name: "manifest refused", PushReference: refusal(http.StatusBadRequest, "MANIFEST_INVALID", 0), Status: http.StatusBadGateway},
	{Name: "throttled", Push: refusal(http.StatusTooManyRequests, "TOOMANYREQUESTS", 9*time.Second), Status: http.StatusServiceUnavailable, RetryAfter: "9"},
	{Name: "busy, without Retry-After", PushReference: refusal(http.StatusServiceUnavailable, "", 0), Status: http.StatusServiceUnavailable, RetryAfter: "1"},
}

// refusal is the error of the registry "local" refusing a push with status and code, after
// asking for a wait of after.
func refusal(status int, code string, after time.Duration) error {
	return &provider.RejectedError{Registry: "local", Status: status, Code: code, RetryAfter: after}
}

// Apply makes f refuse as r.
func (r Refusal) Apply(f *providertest.Fake) {
	f.FailPush, f.FailUpload, f.FailPushReference = r.Push, r.Push, r.PushReference
}

// Check fails t when rr is not the answer of r: its status and Retry-After, and, for a 4xx, a
// message that names the registry by its configured name.
func (r Refusal) Check(t testing.TB, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != r.Status {
		t.Errorf("%s: status %d, want %d; body %s", r.Name, rr.Code, r.Status, rr.Body)
	}
	if got := rr.Header().Get("Retry-After"); got != r.RetryAfter {
		t.Errorf("%s: Retry-After %q, want %q", r.Name, got, r.RetryAfter)
	}
	if r.Status < http.StatusInternalServerError && !strings.Contains(rr.Body.String(), "registry local refused the request") {
		t.Errorf("%s: body %q does not name the registry", r.Name, rr.Body)
	}
}
