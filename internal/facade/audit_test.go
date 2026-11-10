package facade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
)

// viaServer returns a function that sends a manifest PUT by alice to name through the
// server's middleware over e.h, with trusted as server.trustedProxies, from the peer
// 192.0.2.1 with X-Forwarded-For 198.51.100.7 and the request id id, and wants 201.
func (e *env) viaServer(trusted []netip.Prefix) func(id, name, ref string, body []byte) {
	e.t.Helper()
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", Facade: e.h, Log: slog.New(slog.DiscardHandler), TrustedProxies: trusted})
	must(e.t, err)
	return func(id, name, ref string, body []byte) {
		e.t.Helper()
		r := httptest.NewRequest(http.MethodPut, "/v2/"+name+"/manifests/"+ref, bytes.NewReader(body))
		r.RemoteAddr = "192.0.2.1:41000"
		for k, v := range map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Request-Id": id,
			"Content-Type": ocispecManifest, principalHeader: "alice"} {
			r.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, r)
		wantStatus(e.t, "PUT "+name+":"+ref, rr, http.StatusCreated)
	}
}

// pushRow is what TestPushAuditOrigin checks of an audit row.
type pushRow struct {
	Digest    digest.Digest
	IP        string
	RequestID string
}

// pushRows returns the push rows of the audit log, newest first.
func (e *env) pushRows() []pushRow {
	e.t.Helper()
	evs, err := e.st.ListAudit(e.t.Context(), index.AuditFilter{Action: "push"})
	must(e.t, err)
	rows := make([]pushRow, len(evs))
	for i, ev := range evs {
		var details struct {
			RequestID string `json:"requestId"`
		}
		if len(ev.Details) > 0 {
			must(e.t, json.Unmarshal(ev.Details, &details))
		}
		rows[i] = pushRow{Digest: ev.Digest, IP: ev.IP, RequestID: details.RequestID}
	}
	return rows
}

// TestPushAuditOrigin (#283): through the server, the push of a raw file to a managed repo and
// the push of its signature write their audit rows with the client's address, the one
// X-Forwarded-For names behind a trusted proxy and the peer otherwise, and the request id.
func TestPushAuditOrigin(t *testing.T) {
	for _, tt := range []struct {
		name    string
		trusted []netip.Prefix
		want    string
	}{
		{"behind a trusted proxy", []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, "198.51.100.7"},
		{"no trusted proxy", nil, "192.0.2.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, envOptions{})
			put := e.viaServer(tt.trusted)
			f := e.newRawFile("docs/remote.txt", "pushed from afar")
			body, d := f.body(t)
			put("req-file", "raw/assets/"+f.path, f.tag(), body)
			e.pushBlob("raw/assets/"+f.path, ocispec.DescriptorEmptyJSON.Data)
			e.pushBlob("raw/assets/"+f.path, []byte("signature"))
			sig, sigDigest := referrerOf(t, d, cosignBundle, "signature")
			put("req-sig", "raw/assets/"+f.path, sigDigest.String(), sig)
			if got, want := e.pushRows(), []pushRow{{sigDigest, tt.want, "req-sig"}, {d, tt.want, "req-file"}}; !slices.Equal(got, want) {
				t.Errorf("push rows %+v, want %+v", got, want)
			}
		})
	}
}

// auditRow is what TestOCIPushAudit checks of an audit row.
type auditRow struct {
	Principal, Repo, Subject string
	Digest                   digest.Digest
	IP, Details              string
}

// TestOCIPushAudit (#245): through the server behind a trusted proxy, a manifest pushed to an
// oci repo writes one push row in the transaction of oci_repos: alice, the repo, the OCI name as
// the subject, the digest, the forwarded address, the tag and the request id. The repeat of
// the same manifest writes another; a push by digest has no tag; a referrer is audited by
// ingest alone, once; a push the backend refuses writes none.
func TestOCIPushAudit(t *testing.T) {
	e := newEnv(t, envOptions{})
	put := e.viaServer([]netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})
	body, d := manifest(t, []byte("layer"), "first")
	put("req-1", "oci/images/app", "v1", body)
	put("req-2", "oci/images/app", "v1", body)
	byDigest, d2 := manifest(t, []byte("layer"), "second")
	put("req-3", "oci/images/app", d2.String(), byDigest)
	e.pushBlob("oci/images/app", ocispec.DescriptorEmptyJSON.Data)
	e.pushBlob("oci/images/app", []byte("signature"))
	sig, sigDigest := referrerOf(t, d, cosignBundle, "signature")
	put("req-4", "oci/images/app", sigDigest.String(), sig)
	e.prov.FailPushReference = fmt.Errorf("the tag is immutable: %w", provider.ErrConflict)
	other, _ := manifest(t, []byte("layer"), "third")
	wantError(t, "a push the backend refuses", e.do(e.h, http.MethodPut, "/v2/oci/images/app/manifests/v1", bytes.NewReader(other),
		"Content-Type", ocispecManifest, principalHeader, "alice"), http.StatusConflict, codeTagInvalid)

	evs, err := e.st.ListAudit(t.Context(), index.AuditFilter{Action: "push"})
	must(t, err)
	got := make([]auditRow, len(evs))
	for i, ev := range evs {
		got[i] = auditRow{ev.Principal, ev.Repo, ev.Subject, ev.Digest, ev.IP, string(ev.Details)}
	}
	alice := userP("alice").String()
	want := []auditRow{
		{alice, "oci/images", d.String(), sigDigest, "198.51.100.7",
			`{"artifactType":"` + cosignBundle + `","kind":"attestation","requestId":"req-4"}`},
		{alice, "oci/images", "oci/images/app", d2, "198.51.100.7", `{"requestId":"req-3"}`},
		{alice, "oci/images", "oci/images/app", d, "198.51.100.7", `{"requestId":"req-2","tag":"v1"}`},
		{alice, "oci/images", "oci/images/app", d, "198.51.100.7", `{"requestId":"req-1","tag":"v1"}`},
	}
	if !slices.Equal(got, want) {
		t.Errorf("push rows\n%+v\nwant\n%+v", got, want)
	}
	ocis, err := e.st.ListOCIRepos(t.Context(), e.oci.ID)
	must(t, err)
	if len(ocis) != 1 || ocis[0].Path != "oci/images/app" {
		t.Errorf("oci_repos = %+v, want oci/images/app", ocis)
	}
}
