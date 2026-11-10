package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestDeleteTaggedManifest (#284): on an oci repo a manifest pushed under two tags is not deleted
// by one of them: 409 DENIED naming the other in the message and in detail.tags, and both tags
// still name it; by its digest it goes with both, 202.
func TestDeleteTaggedManifest(t *testing.T) {
	e := newEnv(t, envOptions{})
	body, d := manifest(t, []byte("layer"), "tagged")
	target := "/v2/oci/images/app/manifests/"
	for _, tag := range []string{"v1", "latest"} {
		wantStatus(t, "PUT "+tag, e.do(e.h, http.MethodPut, target+tag, bytes.NewReader(body), "Content-Type", ocispecManifest), http.StatusCreated)
	}
	rr := e.do(e.h, http.MethodDelete, target+"v1", nil)
	msg := wantError(t, "DELETE by tag", rr, http.StatusConflict, codeDenied)
	var answer struct {
		Errors []struct {
			Detail struct{ Tags []string } `json:"detail"`
		} `json:"errors"`
	}
	must(t, json.Unmarshal(rr.Body.Bytes(), &answer))
	if !strings.Contains(msg, "also tagged latest") || len(answer.Errors) != 1 || !cmp.Equal(answer.Errors[0].Detail.Tags, []string{"latest"}) {
		t.Errorf("message %q, body %s: want latest named in both", msg, rr.Body)
	}
	for _, tag := range []string{"v1", "latest"} {
		wantStatus(t, "GET "+tag+" after the refusal", e.do(e.h, http.MethodGet, target+tag, nil), http.StatusOK)
	}
	wantStatus(t, "DELETE by digest", e.do(e.h, http.MethodDelete, target+d.String(), nil), http.StatusAccepted)
	for _, tag := range []string{"v1", "latest"} {
		wantError(t, "GET "+tag+" after the delete", e.do(e.h, http.MethodGet, target+tag, nil), http.StatusNotFound, codeManifestUnknown)
	}
}

// stopTags is the fake whose repositories stop in the first Tags until open is closed, having
// closed in.
type stopTags struct {
	*providertest.Fake
	in, open chan struct{}
	once     *sync.Once
}

func (f stopTags) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Fake.Repository(ctx, name)
	return stopTagsRepo{rr, f}, err
}

type stopTagsRepo struct {
	provider.Repository
	f stopTags
}

func (r stopTagsRepo) Tags(ctx context.Context, last string, fn func([]string) error) error {
	r.f.once.Do(func() { close(r.f.in); <-r.f.open })
	return r.Repository.Tags(ctx, last, fn)
}

// TestDeleteTaggedPushWaits: a push by tag of an oci repo's manifest that comes while a delete of
// it by another tag counts the tags waits for the delete (the tag lock, passThrough), and pushes
// the manifest again after it.
func TestDeleteTaggedPushWaits(t *testing.T) {
	stop := stopTags{in: make(chan struct{}), open: make(chan struct{}), once: &sync.Once{}}
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider { stop.Fake = f; return stop }})
	body, _ := manifest(t, []byte("layer"), "raced")
	target := "/v2/oci/images/app/manifests/"
	wantStatus(t, "PUT v1", e.do(e.h, http.MethodPut, target+"v1", bytes.NewReader(body), "Content-Type", ocispecManifest), http.StatusCreated)
	deleted, pushed := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() { deleted <- e.do(e.h, http.MethodDelete, target+"v1", nil) }()
	<-stop.in
	go func() {
		pushed <- e.do(e.h, http.MethodPut, target+"v2", bytes.NewReader(body), "Content-Type", ocispecManifest)
	}()
	select {
	case rr := <-pushed:
		t.Fatalf("the push by tag answered %d while the delete counted the tags", rr.Code)
	case <-time.After(100 * time.Millisecond):
	}
	close(stop.open)
	wantStatus(t, "DELETE v1", <-deleted, http.StatusAccepted)
	wantStatus(t, "PUT v2", <-pushed, http.StatusCreated)
	wantError(t, "GET v1", e.do(e.h, http.MethodGet, target+"v1", nil), http.StatusNotFound, codeManifestUnknown)
	wantStatus(t, "GET v2, pushed after the delete", e.do(e.h, http.MethodGet, target+"v2", nil), http.StatusOK)
}
