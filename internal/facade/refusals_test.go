package facade

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
)

// TestRegistryRefusalsBeyondUploads is #495: a registry that refuses the manifest push of an oci
// repo, a blob or manifest read, or tags/list gets the client the same mapping as a refused blob
// upload (TestRegistryRefusals): the client's own mistake keeps the registry's 4xx and its code
// when the spec has it for a push, else the handler's; a busy registry's 429 or 503 is 503
// UNAVAILABLE with its Retry-After, 1 s when it sent none; nothing else of the registry's.
func TestRegistryRefusalsBeyondUploads(t *testing.T) {
	ra := func(s string) http.Header { return http.Header{"Retry-After": {s}} }
	body, _ := manifest(t, []byte("layer"), "refused")
	blob := digest.FromString("a blob").String()
	const (
		manifestV1 = "/v2/oci/images/app/manifests/v1"
		tagsList   = "/v2/oci/images/app/tags/list"
	)
	reads := []string{http.MethodGet, http.MethodHead}
	tests := []struct {
		name           string
		method, target string   // the client's request
		refuse         []string // the methods the registry refuses on the request's path
		status         int
		code           string
		hdr            http.Header
		want           int
		wantCode       string
		retryAfter     string
		named          bool // the message names the registry
	}{
		{name: "manifest invalid", method: http.MethodPut, target: manifestV1, refuse: []string{http.MethodPut},
			status: 400, code: codeManifestInvalid, want: 400, wantCode: codeManifestInvalid, named: true},
		{name: "manifest blob unknown", method: http.MethodPut, target: manifestV1, refuse: []string{http.MethodPut},
			status: 400, code: codeManifestBlobUnknown, want: 400, wantCode: codeManifestBlobUnknown, named: true},
		{name: "manifest too large for the registry", method: http.MethodPut, target: manifestV1, refuse: []string{http.MethodPut},
			status: 413, want: 413, wantCode: codeManifestInvalid, named: true},
		{name: "manifest push throttled", method: http.MethodPut, target: manifestV1, refuse: []string{http.MethodPut},
			status: 429, code: codeTooManyRequests, hdr: ra("5"), want: 503, wantCode: codeUnavailable, retryAfter: "5"},
		{name: "manifest read throttled", method: http.MethodGet, target: manifestV1, refuse: reads,
			status: 429, want: 503, wantCode: codeUnavailable, retryAfter: "1"},
		{name: "manifest HEAD, registry busy", method: http.MethodHead, target: manifestV1, refuse: reads,
			status: 503, hdr: ra("4"), want: 503, wantCode: codeUnavailable, retryAfter: "4"},
		{name: "blob read throttled", method: http.MethodGet, target: "/v2/oci/images/app/blobs/" + blob, refuse: reads,
			status: 429, code: codeTooManyRequests, hdr: ra("6"), want: 503, wantCode: codeUnavailable, retryAfter: "6"},
		{name: "blob HEAD refused", method: http.MethodHead, target: "/v2/oci/images/app/blobs/" + blob, refuse: reads,
			status: 400, want: 400, wantCode: codeBlobUnknown, named: true},
		{name: "tags/list throttled", method: http.MethodGet, target: tagsList, refuse: reads,
			status: 429, want: 503, wantCode: codeUnavailable, retryAfter: "1"},
		{name: "tags/list, name refused", method: http.MethodGet, target: tagsList, refuse: reads,
			status: 400, code: codeNameInvalid, want: 400, wantCode: codeNameInvalid, named: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := strings.TrimPrefix(tt.target, "/v2/")
			f := &refusingRegistry{status: tt.status, code: tt.code, hdr: tt.hdr,
				refuse: func(r *http.Request, _ int64) bool {
					return slices.Contains(tt.refuse, r.Method) && strings.HasSuffix(r.URL.Path, path)
				}}
			e, _, srv := refusingEnv(t, f)
			var rr *httptest.ResponseRecorder
			if tt.method == http.MethodPut {
				rr = e.do(e.h, tt.method, tt.target, bytes.NewReader(body), "Content-Type", ocispecManifest)
			} else {
				rr = e.do(e.h, tt.method, tt.target, nil)
			}
			wantRefusal(t, tt.name, rr, srv, tt.want, tt.wantCode, tt.named)
			wantHeader(t, tt.name, rr, "Retry-After", tt.retryAfter)
		})
	}
}
