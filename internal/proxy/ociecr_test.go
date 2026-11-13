package proxy_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// awsEnv makes the AWS SDK's default chain the static keys of the test: no profile, no region
// and no instance metadata.
func awsEnv(t *testing.T) {
	none := filepath.Join(t.TempDir(), "none")
	for k, v := range map[string]string{
		"AWS_ACCESS_KEY_ID": "testing", "AWS_SECRET_ACCESS_KEY": "testing", "AWS_SESSION_TOKEN": "",
		"AWS_REGION": "", "AWS_DEFAULT_REGION": "", "AWS_PROFILE": "", "AWS_CONFIG_FILE": none,
		"AWS_SHARED_CREDENTIALS_FILE": none, "AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(k, v)
	}
}

// fakeECRAPI is the ECR API as far as an upstream asks it: GetAuthorizationToken gives
// AWS:token-1 for 12 hours, or AccessDeniedException while denied is set.
type fakeECRAPI struct {
	mu     sync.Mutex
	calls  int
	denied bool
}

func (f *fakeECRAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	denied := f.denied
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	if denied || !strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".GetAuthorizationToken") {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"__type":"AccessDeniedException","message":"not authorized to perform ecr:GetAuthorizationToken"}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"authorizationData": []map[string]any{{
		"authorizationToken": base64.StdEncoding.EncodeToString([]byte("AWS:token-1")),
		"expiresAt":          time.Now().Add(12 * time.Hour).Unix(),
	}}})
}

func (f *fakeECRAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ecrV2 is ECR's /v2 in front of the hub: Basic AWS:token-1, else its Basic challenge.
func ecrV2(t *testing.T, h *hub) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "AWS" || p != "token-1" {
			w.Header().Set("WWW-Authenticate", `Basic realm="https://123456789012.dkr.ecr.us-east-1.amazonaws.com/",service="ecr.amazonaws.com"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.Header.Set("Authorization", "Bearer anon")
		h.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestOCIUpstreamECR: an oci upstream with auth.ecr pulls with ECR's token, which the SDK gets
// from the ECR API of the configuration; the revalidation of a tag uses the same cached token;
// a token that cannot be had is 502 with no request to the upstream; without a region the
// configuration is refused, naming the key (docs/spec/proxy-and-group.md §11.1).
func TestOCIUpstreamECR(t *testing.T) {
	awsEnv(t)
	e := newOCIEnv(t, ociSettings)
	api := &fakeECRAPI{}
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)
	v2 := ecrV2(t, e.hub)
	load := func(region string) (map[string]*proxy.OCIUpstream, error) {
		return proxy.LoadOCIUpstreams([]config.Upstream{{Name: "hub", Format: config.FormatOCI, URL: v2.URL, Retries: -1,
			Auth: config.UpstreamAuth{ECR: &config.UpstreamECR{Region: region, Endpoint: apiSrv.URL}}}}, slog.New(slog.DiscardHandler))
	}
	ups, err := load("us-east-1")
	must(t, err)
	e.up = ups["hub"]
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{})
	desc, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	if err != nil || desc.Digest != img.index.Digest {
		t.Fatalf("OCIManifest = %s, %v; want %s", desc.Digest, err, img.index.Digest)
	}
	e.advance(11 * time.Minute)
	heads := e.hub.count("HEAD /v2/library/nginx/manifests/1.27")
	if _, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27"); err != nil {
		t.Fatalf("OCIManifest of the expired tag: %v", err)
	}
	if n := e.hub.count("HEAD /v2/library/nginx/manifests/1.27"); n != heads+1 {
		t.Errorf("revalidation HEADs with the token = %d, want 1", n-heads)
	}
	if n := api.count(); n != 1 {
		t.Errorf("GetAuthorizationToken called %d times, want once for both clients", n)
	}

	api.mu.Lock()
	api.denied = true
	api.mu.Unlock()
	ups, err = load("us-east-1")
	must(t, err)
	e.up = ups["hub"]
	before := e.hub.count("")
	_, _, err = e.core("b", proxy.Options{}).OCIManifest(t.Context(), e.repo, "library/redis", "7.4")
	if !errors.Is(err, format.ErrBadGateway) || strings.Contains(err.Error(), "token-1") {
		t.Errorf("OCIManifest without a token = %v, want ErrBadGateway", err)
	}
	if n := e.hub.count(""); n != before {
		t.Errorf("%d requests reached the upstream with a token, want none", n-before)
	}

	if _, err := load(""); err == nil || !strings.Contains(err.Error(), "upstreams[0].auth.ecr") || !strings.Contains(err.Error(), "no AWS region") {
		t.Errorf("LoadOCIUpstreams without a region = %v, want an error naming upstreams[0].auth.ecr", err)
	}
}
