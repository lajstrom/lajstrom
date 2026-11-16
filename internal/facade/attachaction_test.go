package facade

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestAttachPush (#89): a principal that may attach and may not write gets push as attach from
// the token endpoint; with that bearer, and with its own credentials, it uploads the blobs of a
// referrer and pushes the referrer, 201. A manifest that is no referrer is 403 DENIED naming
// write, and nothing is stored. A principal with neither write nor attach is 403 for the blobs
// as before.
func TestAttachPush(t *testing.T) {
	e := newEnv(t, envOptions{authz: authorizer{
		"alice": {"raw/assets": {auth.ActionRead, auth.ActionWrite}},
		"scan":  {"raw/assets": {auth.ActionRead, auth.ActionAttach}},
		"bob":   {"raw/assets": {auth.ActionRead}},
	}})
	f, subject := e.signedFile("docs/attach.txt")
	name := "/v2/raw/assets/" + f.path
	tok := e.getToken(e.h, "scan", "repository:raw/assets/"+f.path+":pull,push")
	if access := fmt.Sprint(payload(t, tok)["access"]); access != "[map[actions:[pull attach] name:raw/assets/"+f.path+" type:repository]]" {
		t.Errorf("the token of an attacher: access %s, want pull and attach", access)
	}
	if access := fmt.Sprint(payload(t, e.getToken(e.h, "alice", "repository:raw/assets/"+f.path+":pull,push"))["access"]); !strings.Contains(access, "[pull push]") {
		t.Errorf("the token of a writer: access %s, want pull and push", access)
	}
	next := e.newRawFile(f.path, "the next version of "+f.path)
	version, _ := next.body(t)
	for _, how := range []struct {
		name         string
		artifactType string
		hdr          []string
	}{
		{"its credentials", cosignBundle, []string{principalHeader, "scan"}},
		{"the bearer", "application/vnd.cyclonedx+json", []string{"Authorization", "Bearer " + tok}},
	} {
		rr := e.do(e.h, http.MethodPost, name+"/blobs/uploads/", nil, how.hdr...)
		wantStatus(t, how.name+": a blob upload", rr, http.StatusAccepted)
		ref, d := referrerOf(t, subject, how.artifactType, "signature")
		rr = e.do(e.h, http.MethodPut, name+"/manifests/"+d.String(), bytes.NewReader(ref), append([]string{"Content-Type", ocispecManifest}, how.hdr...)...)
		wantStatus(t, how.name+": a referrer", rr, http.StatusCreated)
		rr = e.do(e.h, http.MethodPut, name+"/manifests/"+next.tag(), bytes.NewReader(version), append([]string{"Content-Type", ocispecManifest}, how.hdr...)...)
		if msg := wantError(t, how.name+": a version", rr, http.StatusForbidden, codeDenied); !strings.Contains(msg, "needs write") {
			t.Errorf("%s: a version refused with %q, want it to name write", how.name, msg)
		}
		wantStatus(t, how.name+": HEAD of the version refused", e.do(e.h, http.MethodHead, name+"/manifests/"+next.tag(), nil, principalHeader, "alice"), http.StatusNotFound)
	}
	rr := e.do(e.h, http.MethodPost, name+"/blobs/uploads/", nil, principalHeader, "bob")
	wantError(t, "a reader's blob upload", rr, http.StatusForbidden, codeDenied)
}
