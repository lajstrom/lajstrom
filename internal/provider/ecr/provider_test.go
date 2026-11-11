package ecr

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestCapabilityDefaults: ECR's capabilities fill what registries[].capabilities leaves unset,
// and immutableTags follows the mutability of the repositories Lajstrom creates.
func TestCapabilityDefaults(t *testing.T) {
	tests := []struct {
		name       string
		set        config.CapabilityOverrides
		mutability types.ImageTagMutability
		want       config.CapabilityOverrides
	}{
		{"defaults", config.CapabilityOverrides{}, types.ImageTagMutabilityImmutable,
			config.CapabilityOverrides{CreateOnPush: aws.Bool(false), Catalog: aws.Bool(true), ImmutableTags: aws.Bool(true), MaxBlobSize: maxLayerSize, Consistency: config.ConsistencyEventual}},
		{"exclusion", config.CapabilityOverrides{}, types.ImageTagMutabilityImmutableWithExclusion,
			config.CapabilityOverrides{CreateOnPush: aws.Bool(false), Catalog: aws.Bool(true), ImmutableTags: aws.Bool(true), MaxBlobSize: maxLayerSize, Consistency: config.ConsistencyEventual}},
		{"mutable", config.CapabilityOverrides{}, types.ImageTagMutabilityMutableWithExclusion,
			config.CapabilityOverrides{CreateOnPush: aws.Bool(false), Catalog: aws.Bool(true), ImmutableTags: aws.Bool(false), MaxBlobSize: maxLayerSize, Consistency: config.ConsistencyEventual}},
		{"set", config.CapabilityOverrides{CreateOnPush: aws.Bool(true), Catalog: aws.Bool(false), Referrers: aws.Bool(false), MaxBlobSize: 1 << 30,
			Consistency: config.ConsistencyStrong},
			types.ImageTagMutabilityImmutable,
			config.CapabilityOverrides{CreateOnPush: aws.Bool(true), Catalog: aws.Bool(false), Referrers: aws.Bool(false), ImmutableTags: aws.Bool(true), MaxBlobSize: 1 << 30,
				Consistency: config.ConsistencyStrong}},
	}
	for _, tt := range tests {
		if diff := cmp.Diff(tt.want, capabilities(tt.set, tt.mutability)); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tt.name, diff)
		}
	}
}

func TestMain(m *testing.M) {
	backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
	os.Exit(m.Run())
}

// awsEnv gives the AWS SDK static credentials and nothing else from the machine: no profile,
// no region, no instance metadata.
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

func TestNewRegion(t *testing.T) {
	awsEnv(t)
	tests := []struct {
		url, region         string
		wantRegion, account string
	}{
		{"https://123456789012.dkr.ecr.eu-west-1.amazonaws.com", "", "eu-west-1", "123456789012"},
		{"https://123456789012.dkr.ecr-fips.us-gov-west-1.amazonaws.com", "", "us-gov-west-1", "123456789012"},
		{"https://123456789012.dkr-ecr.ap-south-1.on.aws", "", "ap-south-1", "123456789012"},
		{"https://123456789012.dkr.ecr.eu-west-1.amazonaws.com", "eu-central-1", "eu-central-1", "123456789012"},
		{"http://localhost:5011", "us-east-1", "us-east-1", ""},
		{"http://localhost:5011", "", "", ""},
	}
	for _, tt := range tests {
		p, err := New(config.Registry{Name: "ecr", Type: Type, URL: tt.url, ECR: config.ECR{Region: tt.region}}, nil)
		if tt.wantRegion == "" {
			if err == nil || !strings.Contains(err.Error(), "no AWS region") {
				t.Errorf("%s: New error = %v, want no AWS region", tt.url, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: New: %v", tt.url, err)
		}
		region := p.api.(*ecr.Client).Options().Region
		account := ""
		if p.registryID != nil {
			account = *p.registryID
		}
		if region != tt.wantRegion || account != tt.account {
			t.Errorf("%s: region %q, account %q; want %q, %q", tt.url, region, account, tt.wantRegion, tt.account)
		}
	}
}

// fakeAWS is the ECR API endpoint over HTTP for the SDK itself: GetAuthorizationToken, of
// which the first throttled answers are ThrottlingException.
type fakeAWS struct {
	mu        sync.Mutex
	throttled int // answers left that are throttled
	targets   []string
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")
	f.mu.Lock()
	f.targets = append(f.targets, target)
	throttle := f.throttled > 0
	f.throttled--
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	switch {
	case throttle:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"__type":"ThrottlingException","message":"Rate exceeded"}`)
	case strings.HasSuffix(target, ".GetAuthorizationToken"):
		_ = json.NewEncoder(w).Encode(map[string]any{"authorizationData": []map[string]any{{
			"authorizationToken": base64.StdEncoding.EncodeToString([]byte("AWS:token-1")),
			"expiresAt":          time.Now().Add(12 * time.Hour).Unix(),
		}}})
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"__type":"ValidationException","message":"not in the fake"}`)
	}
}

// ecrV2 answers /v2 as ECR does, as far as the test goes: Basic credentials from the token,
// NAME_UNKNOWN for the referrers of a repository that does not exist, and TAG_INVALID for a tag
// of an immutable repository that is pushed again (the answer the AWS documentation shows; to
// be measured by #19).
func ecrV2(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != "AWS" || p != "token-1" {
		w.Header().Set("WWW-Authenticate", `Basic realm="https://123456789012.dkr.ecr.us-east-1.amazonaws.com/",service="ecr.amazonaws.com"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	answer := func(status int, code, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"errors":[{"code":%q,"message":%q}]}`, code, msg)
	}
	switch {
	case r.URL.Path == "/v2/":
		_, _ = io.WriteString(w, "{}")
	case strings.Contains(r.URL.Path, "/referrers/"):
		answer(http.StatusNotFound, "NAME_UNKNOWN", "The repository with name 'art/lajstrom-probe' does not exist")
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/"):
		answer(http.StatusBadRequest, "TAG_INVALID", "The image tag 'v1' already exists in the 'art/raw/scratch/notes' repository and cannot be overwritten because the repository is immutable.")
	default:
		answer(http.StatusNotFound, "NOT_FOUND", "")
	}
}

// TestDataPath runs the provider with the SDK against fake ECR endpoints: the token, fetched
// once past a throttled answer, is the Basic credential of /v2; ECR's capabilities are those
// of the configuration's defaults; a tag pushed again to an immutable repository is a conflict.
func TestDataPath(t *testing.T) {
	awsEnv(t)
	ctx := t.Context()
	api := &fakeAWS{throttled: 2}
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)
	v2 := httptest.NewServer(http.HandlerFunc(ecrV2))
	t.Cleanup(v2.Close)
	cfg := config.Registry{Name: "ecr", Type: Type, URL: v2.URL, Prefix: "art/",
		ECR: config.ECR{Region: "us-east-1", Endpoint: apiSrv.URL}}
	p, err := provider.New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	caps, err := p.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	want := provider.Capabilities{Catalog: true, DeleteManifest: true, ImmutableTags: true, BlobRedirect: true, MaxBlobSize: maxLayerSize,
		Consistency: provider.ConsistencyEventual}
	if caps != want {
		t.Errorf("Capabilities = %+v, want %+v", caps, want)
	}
	if err := p.(provider.Pinger).Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	api.mu.Lock()
	targets := strings.Join(api.targets, " ")
	api.mu.Unlock()
	if want := strings.Repeat("AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken ", 3); targets+" " != want {
		t.Errorf("ECR API calls: %s; want two throttled GetAuthorizationToken and one more", targets)
	}

	repo, err := p.Repository(ctx, "raw/scratch/notes")
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest))}
	err = repo.PushReference(ctx, desc, bytes.NewReader(manifest), "v1")
	wantErr(t, "push to an immutable tag", err, provider.ErrConflict)
	if err != nil && strings.Contains(err.Error(), "art/") {
		t.Errorf("error %q shows the instance prefix", err)
	}
}
