package facade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Delete is the stub's answer to a delete: its error.
func (s stubIngester) Delete(context.Context, index.Repo, format.Deletion, auth.Principal) (*format.Deleted, error) {
	return nil, s.err
}

// TestDeleteManifest: a raw file deleted through /v2 by its tag and by its digest: 202, its
// signature gone first, the manifest unknown after, the version gone from tags/list and from
// the index, one audit row each that counts the referrers.
func TestDeleteManifest(t *testing.T) {
	for _, byDigest := range []bool{false, true} {
		e := newEnv(t, envOptions{})
		f, subject := e.signedFile("docs/gone.txt")
		sig, sigDigest := referrerOf(t, subject, cosignBundle, "signature")
		wantStatus(t, "push of the signature", e.put(f, sigDigest.String(), sig), http.StatusCreated)
		ref, name := f.tag(), "/v2/raw/assets/"+f.path
		if byDigest {
			ref = subject.String()
		}
		rr := e.do(e.h, http.MethodDelete, name+"/manifests/"+ref, nil, principalHeader, "alice")
		wantStatus(t, "DELETE", rr, http.StatusAccepted)
		if rr.Body.Len() != 0 {
			t.Errorf("DELETE body %q", rr.Body)
		}
		for _, d := range []string{subject.String(), sigDigest.String()} {
			wantError(t, "GET "+d, e.do(e.h, http.MethodGet, name+"/manifests/"+d, nil, principalHeader, "alice"), http.StatusNotFound, codeManifestUnknown)
		}
		tags := e.do(e.h, http.MethodGet, name+"/tags/list", nil, principalHeader, "alice")
		wantStatus(t, "tags/list", tags, http.StatusOK)
		if strings.Contains(tags.Body.String(), f.tag()) {
			t.Errorf("tags/list after the delete: %s", tags.Body)
		}
		if _, err := e.st.GetPackage(t.Context(), e.raw.ID, f.path); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("the package is still in the index: %v", err)
		}
		audit, err := e.st.ListAudit(t.Context(), index.AuditFilter{})
		must(t, err)
		var details map[string]int
		must(t, json.Unmarshal(audit[0].Details, &details))
		if ev := audit[0]; ev.Action != "delete" || ev.Digest != subject || ev.Principal != userP("alice").String() || details["referrers"] != 1 {
			t.Errorf("by digest %t: audit event %+v, details %v", byDigest, ev, details)
		}
		wantError(t, "DELETE again", e.do(e.h, http.MethodDelete, name+"/manifests/"+ref, nil, principalHeader, "alice"),
			http.StatusNotFound, codeManifestUnknown)
	}
}

// TestDeleteManifestErrors is the error table of a delete (deleteError), with the pipeline's
// errors as they come, the references the facade refuses itself, and a backend that cannot
// delete manifests, which keeps the version.
func TestDeleteManifestErrors(t *testing.T) {
	target := "/v2/raw/assets/docs/x.txt/manifests/"
	for _, tt := range []struct {
		name, ref, code string
		err             error
		status          int
	}{
		{"unsupported", "v1", codeUnsupported, fmt.Errorf("ingest: %w", provider.ErrUnsupported), http.StatusMethodNotAllowed},
		{"not found", "v1", codeManifestUnknown, fmt.Errorf("%w: no manifest", index.ErrNotFound), http.StatusNotFound},
		{"refused one", "v1", codeDenied, fmt.Errorf("ingest: %w", provider.ErrConflict), http.StatusConflict},
		{"backend credentials", "v1", codeUnknown, fmt.Errorf("ingest: %w", provider.ErrUnauthorized), http.StatusBadGateway},
		{"anything else", "v1", codeUnknown, errors.New("ingest: https://backend.example went away"), http.StatusInternalServerError},
		{"a tag that is not one", "-v1", codeTagInvalid, nil, http.StatusBadRequest},
		{"a digest that is not one", "sha256:00", codeDigestInvalid, nil, http.StatusBadRequest},
	} {
		e := newEnv(t, envOptions{ingest: stubIngester{tt.err}})
		msg := wantError(t, tt.name, e.do(e.h, http.MethodDelete, target+tt.ref, nil, principalHeader, "alice"), tt.status, tt.code)
		if strings.Contains(msg, "backend.example") {
			t.Errorf("%s: message %q", tt.name, msg)
		}
	}
	e := newEnv(t, envOptions{})
	f, subject := e.signedFile("docs/kept.txt")
	e.prov.NoDeleteManifest = true
	msg := wantError(t, "a backend without deletes", e.do(e.h, http.MethodDelete, "/v2/raw/assets/"+f.path+"/manifests/"+f.tag(), nil,
		principalHeader, "alice"), http.StatusMethodNotAllowed, codeUnsupported)
	if !strings.Contains(msg, `registry "local"`) {
		t.Errorf("message %q does not name the registry", msg)
	}
	e.wantIndexed(f, subject)
}

// TestDeleteManifestAccess: delete needs the action on the repo, checked before the reference:
// 403 DENIED to a principal that can read the repo, NAME_UNKNOWN to one that cannot, 401 to
// anonymous; a token with pull only is 403 too. Blobs stay 405 for one that may delete.
func TestDeleteManifestAccess(t *testing.T) {
	e := newEnv(t, envOptions{authz: authorizer{
		"alice": {"raw/assets": {auth.ActionRead, auth.ActionWrite, auth.ActionDelete}},
		"bob":   {"raw/assets": {auth.ActionRead, auth.ActionWrite}},
	}})
	f, subject := e.signedFile("docs/guarded.txt")
	target := "/v2/raw/assets/" + f.path + "/manifests/" + f.tag()
	for _, tt := range []struct {
		name   string
		hdr    []string
		status int
		code   string
	}{
		{"a writer", []string{principalHeader, "bob"}, http.StatusForbidden, codeDenied},
		{"a user without access", []string{principalHeader, "carol"}, http.StatusNotFound, codeNameUnknown},
		{"anonymous", nil, http.StatusUnauthorized, codeUnauthorized},
		{"a pull and push token", []string{"Authorization", "Bearer " + e.issue(userP("alice"), repoAccess("raw/assets/"+f.path, "pull", "push"))},
			http.StatusForbidden, codeDenied},
	} {
		wantError(t, tt.name, e.do(e.h, http.MethodDelete, target, nil, tt.hdr...), tt.status, tt.code)
	}
	e.wantIndexed(f, subject)
	wantError(t, "DELETE of a blob", e.do(e.h, http.MethodDelete, "/v2/raw/assets/"+f.path+"/blobs/"+subject.String(), nil,
		principalHeader, "alice"), http.StatusMethodNotAllowed, codeUnsupported)
	tok := e.issue(userP("alice"), repoAccess("raw/assets/"+f.path, "delete"))
	wantStatus(t, "a delete token", e.do(e.h, http.MethodDelete, target, nil, "Authorization", "Bearer "+tok), http.StatusAccepted)
}
