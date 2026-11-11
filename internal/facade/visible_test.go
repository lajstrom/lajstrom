package facade

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
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
