package facade

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

func TestParseRoute(t *testing.T) {
	tests := []struct {
		path string
		want route
		ok   bool
	}{
		{"oci/images/a/manifests/v1", route{kind: routeManifest, name: "oci/images/a", arg: "v1"}, true},
		{"oci/images/a/b/blobs/sha256:00", route{kind: routeBlob, name: "oci/images/a/b", arg: "sha256:00"}, true},
		{"oci/images/a/blobs/uploads/", route{kind: routeUploads, name: "oci/images/a"}, true},
		{"oci/images/a/blobs/uploads", route{kind: routeUploads, name: "oci/images/a"}, true},
		{"oci/images/a/blobs/uploads/0f", route{kind: routeUpload, name: "oci/images/a", arg: "0f"}, true},
		{"oci/images/blobs/blobs/uploads/0f", route{kind: routeUpload, name: "oci/images/blobs", arg: "0f"}, true},
		{"oci/images/manifests/manifests/v1", route{kind: routeManifest, name: "oci/images/manifests", arg: "v1"}, true},
		{"oci/images/a/tags/list", route{kind: routeTags, name: "oci/images/a"}, true},
		{"oci/images/a/referrers/sha256:00", route{kind: routeReferrers, name: "oci/images/a", arg: "sha256:00"}, true},
		{"oci/images/a/blobs/", route{kind: routeBlob, name: "oci/images/a"}, true},
		{"oci/images/a", route{}, false},
		{"token", route{}, false},
		{"a/b", route{}, false},
		{"oci/images/a/tags", route{}, false},
	}
	for _, tt := range tests {
		got, ok := parseRoute(tt.path)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseRoute(%q) = %+v, %v; want %+v, %v", tt.path, got, ok, tt.want, tt.ok)
		}
	}
}

// TestBase checks GET /v2/: 401 with the bearer challenge, without a scope, to the anonymous
// principal, with or without an Authorizer, and 200 {} to an authenticated one.
func TestBase(t *testing.T) {
	for _, z := range []auth.Authorizer{nil, authorizer{}} {
		e := newEnv(t, envOptions{authz: z})
		rr := e.do(e.h, http.MethodGet, "/v2/", nil)
		wantError(t, "anonymous GET /v2/", rr, http.StatusUnauthorized, codeUnauthorized)
		wantHeader(t, "anonymous GET /v2/", rr, "WWW-Authenticate", `Bearer realm="http://example.com/v2/token",service="example.com"`)
		wantHeader(t, "anonymous GET /v2/", rr, "Docker-Distribution-API-Version", "registry/2.0")
		wantStatus(t, "anonymous HEAD /v2/", e.do(e.h, http.MethodHead, "/v2/", nil), http.StatusUnauthorized)

		rr = e.do(e.h, http.MethodGet, "/v2/", nil, principalHeader, "alice")
		wantStatus(t, "GET /v2/ as alice", rr, http.StatusOK)
		wantHeader(t, "GET /v2/ as alice", rr, "Docker-Distribution-API-Version", "registry/2.0")
		if body := strings.TrimSpace(rr.Body.String()); body != "{}" {
			t.Errorf("GET /v2/ body %q", body)
		}
		wantStatus(t, "HEAD /v2/ as alice", e.do(e.h, http.MethodHead, "/v2/", nil, principalHeader, "alice"), http.StatusOK)
		wantError(t, "POST /v2/", e.do(e.h, http.MethodPost, "/v2/", nil, principalHeader, "alice"), http.StatusMethodNotAllowed, codeUnsupported)
	}
}

// TestRouteErrors covers the error codes of the routing table: names, references, methods,
// and the routes whose issues come later.
func TestRouteErrors(t *testing.T) {
	e := newEnv(t, envOptions{})
	long := "oci/images/" + strings.Repeat("a", maxNameLen)
	tests := []struct {
		name, method, target string
		status               int
		code                 string
		message              string // a part of the message; "" skips
	}{
		{"name not the grammar", http.MethodGet, "/v2/Raw/assets/x/tags/list", http.StatusBadRequest, codeNameInvalid, `"Raw/assets/x"`},
		{"name with a double slash", http.MethodGet, "/v2/raw/assets//x/tags/list", http.StatusBadRequest, codeNameInvalid, ""},
		{"name too long", http.MethodGet, "/v2/" + long + "/tags/list", http.StatusBadRequest, codeNameInvalid, ""},
		{"repo unknown", http.MethodGet, "/v2/raw/none/x/tags/list", http.StatusNotFound, codeNameUnknown, "raw/none/x"},
		{"format unknown", http.MethodGet, "/v2/apt/assets/x/manifests/v1", http.StatusNotFound, codeNameUnknown, ""},
		{"no path below the repo", http.MethodGet, "/v2/raw/assets/tags/list", http.StatusNotFound, codeNameUnknown, "<format>/<repo>/<path>"},
		{"no endpoint", http.MethodGet, "/v2/raw/assets/x", http.StatusNotFound, codeNameUnknown, "not an endpoint"},
		{"blob digest invalid", http.MethodGet, "/v2/raw/assets/x/blobs/sha256:nothex", http.StatusBadRequest, codeDigestInvalid, ""},
		{"blob digest empty", http.MethodHead, "/v2/raw/assets/x/blobs/", http.StatusBadRequest, codeDigestInvalid, ""},
		{"manifest digest invalid", http.MethodGet, "/v2/raw/assets/x/manifests/sha256:totallywrong", http.StatusBadRequest, codeDigestInvalid, ""},
		{"read of a tag that is not one", http.MethodGet, "/v2/raw/assets/x/manifests/.INVALID_MANIFEST_NAME", http.StatusNotFound, codeManifestUnknown, "not a tag"},
		{"read of a tag too long", http.MethodHead, "/v2/raw/assets/x/manifests/" + strings.Repeat("t", 129), http.StatusNotFound, codeManifestUnknown, ""},
		{"push to a tag that is not one", http.MethodPut, "/v2/oci/images/x/manifests/.dot", http.StatusBadRequest, codeTagInvalid, ""},
		{"method of no route", http.MethodPost, "/v2/raw/assets/x/manifests/v1", http.StatusMethodNotAllowed, codeUnsupported, "POST"},
		{"PATCH without an upload id", http.MethodPatch, "/v2/raw/assets/x/blobs/uploads/", http.StatusMethodNotAllowed, codeUnsupported, ""},
		{"POST to an upload id", http.MethodPost, "/v2/raw/assets/x/blobs/uploads/00", http.StatusMethodNotAllowed, codeUnsupported, ""},
		{"catalog method", http.MethodPut, "/v2/_catalog", http.StatusMethodNotAllowed, codeUnsupported, ""},
		{"delete manifest", http.MethodDelete, "/v2/oci/images/x/manifests/v1", http.StatusMethodNotAllowed, codeUnsupported, "#61"},
		{"delete blob", http.MethodDelete, "/v2/oci/images/x/blobs/" + string(zeroDigest), http.StatusMethodNotAllowed, codeUnsupported, "#61"},
		{"referrers", http.MethodGet, "/v2/oci/images/x/referrers/" + string(zeroDigest), http.StatusNotImplemented, codeUnsupported, "#28"},
		{"manifest without a Content-Type", http.MethodPut, "/v2/raw/assets/x/manifests/v1", http.StatusBadRequest, codeManifestInvalid, "Content-Type"},
		{"tags n invalid", http.MethodGet, "/v2/oci/images/x/tags/list?n=-1", http.StatusBadRequest, codeUnsupported, "n="},
		{"catalog n invalid", http.MethodGet, "/v2/_catalog?n=x", http.StatusBadRequest, codeUnsupported, "n="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := e.do(e.h, tt.method, tt.target, nil)
			msg := wantError(t, tt.name, rr, tt.status, tt.code)
			if !strings.Contains(msg, tt.message) {
				t.Errorf("message %q does not contain %q", msg, tt.message)
			}
			wantHeader(t, tt.name, rr, "Docker-Distribution-API-Version", "registry/2.0")
		})
	}
}

// TestServerErrors checks that a 5xx says its status text and nothing of its cause, which is
// logged: a registry that is not configured, a backend that fails.
func TestServerErrors(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	e.raw.Registry = "gone"
	must(t, e.st.UpdateRepo(ctx, &e.raw))
	rr := e.do(e.h, http.MethodGet, "/v2/raw/assets/x/manifests/v1", nil)
	if msg := wantError(t, "registry not configured", rr, http.StatusInternalServerError, codeUnknown); msg != "Internal Server Error" {
		t.Errorf("message %q", msg)
	}
	if !strings.Contains(e.logs.String(), `registry \"gone\" is not configured`) {
		t.Errorf("the cause is not logged:\n%s", e.logs)
	}

	e = newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider { return failingRegistry{f} }})
	rr = e.do(e.h, http.MethodGet, "/v2/oci/images/x/manifests/v1", nil)
	if msg := wantError(t, "backend rejects our credentials", rr, http.StatusBadGateway, codeUnknown); msg != "Bad Gateway" {
		t.Errorf("message %q", msg)
	}
	if strings.Contains(rr.Body.String(), "backend.example") || !strings.Contains(e.logs.String(), "backend.example") {
		t.Errorf("the backend shows in the answer %q, or the cause is not logged", rr.Body)
	}
}

// TestAuthorization checks the answers of the facade with an Authorizer to requests without a
// bearer: anonymous 401 with the bearer challenge naming the scope of the request, a reader
// that writes 403, a stranger 404 as for a repo that does not exist, and the catalog filtered.
func TestAuthorization(t *testing.T) {
	z := authorizer{
		"reader": {"oci/images": {auth.ActionRead}},
		"writer": {"oci/images": {auth.ActionRead, auth.ActionWrite}, "raw/assets": {auth.ActionRead}},
	}
	e := newEnv(t, envOptions{authz: z})
	body, _ := manifest(t, []byte("layer"), "authz")
	const realm = `Bearer realm="http://example.com/v2/token",service="example.com"`
	tests := []struct {
		name, user, method, target string
		status                     int
		code                       string
		challenge                  string // of a 401
	}{
		{"anonymous read", "", http.MethodGet, "/v2/oci/images/x/tags/list", http.StatusUnauthorized, codeUnauthorized,
			realm + `,scope="repository:oci/images/x:pull"`},
		{"anonymous on a repo that does not exist", "", http.MethodGet, "/v2/oci/none/x/tags/list", http.StatusUnauthorized, codeUnauthorized,
			realm + `,scope="repository:oci/none/x:pull"`},
		{"reader writes", "reader", http.MethodPost, "/v2/oci/images/x/blobs/uploads/", http.StatusForbidden, codeDenied, ""},
		{"reader pushes a manifest", "reader", http.MethodPut, "/v2/oci/images/x/manifests/v1", http.StatusForbidden, codeDenied, ""},
		{"reader deletes", "reader", http.MethodDelete, "/v2/oci/images/x/manifests/v1", http.StatusForbidden, codeDenied, ""},
		{"anonymous pushes to a managed repo", "", http.MethodPut, "/v2/raw/assets/x/manifests/v1", http.StatusUnauthorized, codeUnauthorized,
			realm + `,scope="repository:raw/assets/x:pull,push"`},
		{"anonymous deletes", "", http.MethodDelete, "/v2/oci/images/x/manifests/v1", http.StatusUnauthorized, codeUnauthorized,
			realm + `,scope="repository:oci/images/x:delete"`},
		{"anonymous mounts", "", http.MethodPost, "/v2/oci/images/x/blobs/uploads/?mount=" + string(zeroDigest) + "&from=raw/assets/y",
			http.StatusUnauthorized, codeUnauthorized, realm + `,scope="repository:oci/images/x:pull,push repository:raw/assets/y:pull"`},
		{"reader pushes to a managed repo", "writer", http.MethodPut, "/v2/raw/assets/x/manifests/v1", http.StatusForbidden, codeDenied, ""},
		{"stranger pushes to a managed repo", "reader", http.MethodPut, "/v2/raw/assets/x/manifests/v1", http.StatusNotFound, codeNameUnknown, ""},
		{"stranger reads", "reader", http.MethodGet, "/v2/raw/assets/x/tags/list", http.StatusNotFound, codeNameUnknown, ""},
		{"stranger reads a blob", "reader", http.MethodGet, "/v2/raw/assets/x/blobs/" + string(zeroDigest), http.StatusNotFound, codeNameUnknown, ""},
		{"writer's upload status of an unknown id", "writer", http.MethodGet, "/v2/oci/images/x/blobs/uploads/" + strings.Repeat("0", 32), http.StatusNotFound, codeBlobUploadUnknown, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := e.do(e.h, tt.method, tt.target, strings.NewReader(string(body)), principalHeader, tt.user,
				"Content-Type", "application/vnd.oci.image.manifest.v1+json")
			wantError(t, tt.name, rr, tt.status, tt.code)
			wantHeader(t, tt.name, rr, "WWW-Authenticate", tt.challenge)
		})
	}
	rr := e.do(e.h, http.MethodPut, "/v2/oci/images/x/manifests/v1", strings.NewReader(string(body)), principalHeader, "writer",
		"Content-Type", ocispecManifest)
	wantStatus(t, "writer pushes a manifest", rr, http.StatusCreated)
	wantStatus(t, "anonymous GET /v2/", e.do(e.h, http.MethodGet, "/v2/", nil), http.StatusUnauthorized)
}
