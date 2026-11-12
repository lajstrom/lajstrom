package facade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestManifestHookNotVisible: a manifest the registry took and does not serve within the
// visibility timeout is 503 UNAVAILABLE with Retry-After, whose message and detail name what it
// does not serve, the manifest and its tag, by the name the client pushed to (#565); the same
// PUT again, once the registry serves it, is 201.
func TestManifestHookNotVisible(t *testing.T) {
	var lag *providertest.Lag
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider {
		lag = &providertest.Lag{Fake: f, Manifests: -1}
		return lag
	}})
	e.ingest = ingest.New(ingest.Options{Providers: provider.Map{"local": e.reg}, Index: e.st,
		Log: slog.New(slog.NewTextHandler(e.logs, nil)), Now: e.clock.Now, VisibilityTimeout: 100 * time.Millisecond})
	e.h = e.replica()
	f := e.newRawFile("docs/e.txt", "not served yet")
	body, d := f.body(t)
	rr := e.put(f, f.tag(), body)
	msg := wantError(t, "a manifest the registry does not serve", rr, http.StatusServiceUnavailable, codeUnavailable)
	wantHeader(t, "the 503", rr, "Retry-After", "30")
	name := "raw/assets/docs/e.txt"
	if want := "the registry took " + name + "@" + d.String() + " and does not serve the manifest, the tag " + f.tag() +
		" yet; the same push again completes it"; msg != want {
		t.Errorf("message %q, want %q", msg, want)
	}
	wantNotServed(t, rr, notServedDetail{Name: name, Digest: d, Missing: missingDetail{Manifest: true, Tag: f.tag()}})
	lag.Serve()
	wantStatus(t, "the same PUT again", e.put(f, f.tag(), body), http.StatusCreated)
}

// TestReferrerNotServed: a referrer that the registry does not list among its subject's
// referrers in time (ingestReferrer) is 503 UNAVAILABLE with Retry-After, whose message and
// detail name it by the name the client pushed to, not by the backend's (#565).
func TestReferrerNotServed(t *testing.T) {
	subject := digest.FromString("an image")
	sig, d := referrerOf(t, subject, cosignBundle, "signature")
	e := newEnv(t, envOptions{ingest: stubIngester{&ingest.NotVisibleError{Name: "backend-base/app", Digest: d, Subject: subject,
		Waited: time.Minute}}})
	e.pushBlob("oci/images/app", ocispec.DescriptorEmptyJSON.Data)
	e.pushBlob("oci/images/app", []byte("signature"))
	rr := e.do(e.h, http.MethodPut, "/v2/oci/images/app/manifests/"+d.String(), bytes.NewReader(sig), "Content-Type", ocispecManifest)
	msg := wantError(t, "a referrer the registry does not list", rr, http.StatusServiceUnavailable, codeUnavailable)
	wantHeader(t, "the 503", rr, "Retry-After", "30")
	if want := "the registry took oci/images/app@" + d.String() + " and does not serve the referrers of " + subject.String() +
		" yet; the same push again completes it"; msg != want {
		t.Errorf("message %q, want %q", msg, want)
	}
	wantNotServed(t, rr, notServedDetail{Name: "oci/images/app", Digest: d, Missing: missingDetail{ReferrersOf: subject}})
}

// TestNotServedBounded: the answer to a push the registry does not serve yet names at most
// maxNamedBlobs of the blobs it does not serve, in the message and in the detail, and counts
// the rest, so that a manifest at the layer bound gets a small answer, not every digest twice
// (#693); a push with fewer blobs missing is answered as before, each named.
func TestNotServedBounded(t *testing.T) {
	blobs := func(n int) []digest.Digest {
		ds := make([]digest.Digest, n)
		for i := range ds {
			ds[i] = digest.FromString(fmt.Sprint("layer ", i))
		}
		slices.Sort(ds)
		return ds
	}
	named := func(ds []digest.Digest) string {
		var b strings.Builder
		for _, d := range ds {
			b.WriteString(", the blob " + d.String())
		}
		return b.String()
	}
	few, many := blobs(3), blobs(maxLayers+1) // a manifest at the layer bound, and its config
	rest := len(many) - maxNamedBlobs
	subject := digest.FromString("an image")
	tests := []struct {
		name    string
		nv      ingest.NotVisibleError
		missing string // what the message names after "does not serve "
		detail  missingDetail
	}{
		{name: "few blobs", nv: ingest.NotVisibleError{Manifest: true, Tag: "t1", Blobs: few},
			missing: "the manifest, the tag t1" + named(few),
			detail:  missingDetail{Manifest: true, Tag: "t1", Blobs: few}},
		{name: "as many as are named", nv: ingest.NotVisibleError{Blobs: many[:maxNamedBlobs]},
			missing: strings.TrimPrefix(named(many[:maxNamedBlobs]), ", "),
			detail:  missingDetail{Blobs: many[:maxNamedBlobs]}},
		{name: "one more", nv: ingest.NotVisibleError{Tag: "t1", Blobs: many[:maxNamedBlobs+1]},
			missing: "the tag t1" + named(many[:maxNamedBlobs]) + ", 1 more blob",
			detail:  missingDetail{Tag: "t1", Blobs: many[:maxNamedBlobs], MoreBlobs: 1}},
		{name: "a manifest at the layer bound", nv: ingest.NotVisibleError{Manifest: true, Tag: "t1", Blobs: many},
			missing: "the manifest, the tag t1" + named(many[:maxNamedBlobs]) + fmt.Sprintf(", %d more blobs", rest),
			detail:  missingDetail{Manifest: true, Tag: "t1", Blobs: many[:maxNamedBlobs], MoreBlobs: rest}},
		{name: "a referrer", nv: ingest.NotVisibleError{Blobs: many, Subject: subject},
			missing: strings.TrimPrefix(named(many[:maxNamedBlobs]), ", ") + fmt.Sprintf(", %d more blobs, the referrers of %s", rest, subject),
			detail:  missingDetail{Blobs: many[:maxNamedBlobs], MoreBlobs: rest, ReferrersOf: subject}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nv := tt.nv
			nv.Name, nv.Digest, nv.Waited = "backend-base/docs/e.txt", digest.FromString("the manifest"), time.Minute
			e := newEnv(t, envOptions{ingest: stubIngester{fmt.Errorf("ingest: push: %w", &nv)}})
			f := e.newRawFile("docs/e.txt", "not served yet")
			body, _ := f.body(t)
			rr := e.put(f, "t1", body)
			msg := wantError(t, tt.name, rr, http.StatusServiceUnavailable, codeUnavailable)
			wantHeader(t, "the 503", rr, "Retry-After", "30")
			if want := "the registry took raw/assets/docs/e.txt@" + nv.Digest.String() + " and does not serve " + tt.missing +
				" yet; the same push again completes it"; msg != want {
				t.Errorf("message %q, want %q", msg, want)
			}
			wantNotServed(t, rr, notServedDetail{Name: "raw/assets/docs/e.txt", Digest: nv.Digest, Missing: tt.detail})
			if more := strings.Contains(rr.Body.String(), `"moreBlobs"`); more != (tt.detail.MoreBlobs > 0) {
				t.Errorf("moreBlobs in the answer: %v, want %v: %s", more, tt.detail.MoreBlobs > 0, rr.Body)
			}
			if rr.Body.Len() > 4<<10 {
				t.Errorf("the answer has %d bytes, want at most 4 KiB", rr.Body.Len())
			}
		})
	}
}

// wantNotServed checks that the answer rr has one error whose detail is want (notServed).
func wantNotServed(t *testing.T, rr *httptest.ResponseRecorder, want notServedDetail) {
	t.Helper()
	var answer struct {
		Errors []struct {
			Detail notServedDetail `json:"detail"`
		} `json:"errors"`
	}
	must(t, json.Unmarshal(rr.Body.Bytes(), &answer))
	if len(answer.Errors) != 1 || !reflect.DeepEqual(answer.Errors[0].Detail, want) {
		t.Errorf("detail of %s, want %+v", rr.Body, want)
	}
}
