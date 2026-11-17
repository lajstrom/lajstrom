package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// reviewBody returns the body of POST /api/v1/access-reviews of the document doc, with note and,
// when scope is not "", the scope.
func reviewBody(t *testing.T, doc []byte, scope, note string) string {
	t.Helper()
	var rep struct {
		GeneratedAt string `json:"generatedAt"`
	}
	must(t, json.Unmarshal(doc, &rep))
	sum := sha256.Sum256(doc)
	body := map[string]string{"sha256": hex.EncodeToString(sum[:]), "generatedAt": rep.GeneratedAt, "note": note}
	if scope != "" {
		body["scope"] = scope
	}
	b, err := json.Marshal(body)
	must(t, err)
	return string(b)
}

// TestAccessReviewRecorded: an admin reads the report and records its review by the sha256 of
// the bytes it got: 201 with the row access.review as GET /api/v1/audit answers it, by the admin,
// subject sha256:<hex>, its details the body's with the origin of the API; the scope is all when
// absent, a note may hold line feeds, and no changeReason is asked while auth.requireReason is
// on. The server keeps nothing but the row; past reviews are GET /api/v1/audit?action=access.review,
// newest first.
func TestAccessReviewRecorded(t *testing.T) {
	f, _ := accessFixture(t, Options{RequireReason: true})
	root := auth.Principal{Kind: auth.KindUser, Name: "root"}
	rr := f.do(http.MethodGet, "/api/v1/access-report?scope=people", "", root)
	wantStatus(t, rr, http.StatusOK)
	doc := rr.Body.Bytes()
	sum := sha256.Sum256(doc)
	before := len(auditOf(t, f.st, ""))

	note := "Reviewed with ops.\nRemoved bob's token. Acknowledged: CISO, CHG-1279."
	var first AuditEntry
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/access-reviews", reviewBody(t, doc, "people", note), root), http.StatusCreated, &first)
	var details map[string]any
	must(t, json.Unmarshal(first.Details, &details))
	var rep AccessReport
	must(t, json.Unmarshal(doc, &rep))
	uid := f.uidOf(t, "root")
	if first.Action != auth.ActionAccessReview || first.Subject != "sha256:"+hex.EncodeToString(sum[:]) || first.Repo != "" ||
		first.Principal != "user:"+uid || first.PrincipalName != "root" || first.State != index.AuditOK {
		t.Errorf("the row %+v", first)
	}
	wantJSON(t, "its details", details, fmt.Sprintf(`{"sha256": %q, "generatedAt": %q, "scope": "people", "note": %q, "source": "api"}`,
		hex.EncodeToString(sum[:]), rep.GeneratedAt.Format(time.RFC3339), note))

	var second AuditEntry
	body := reviewBody(t, doc, "", "Second look.")
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/access-reviews", body, root), http.StatusCreated, &second)
	var list AuditList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/audit?action=access.review", "", root), http.StatusOK, &list)
	if len(list.Events) != 2 || list.Events[0].ID != second.ID || list.Events[1].ID != first.ID {
		t.Fatalf("GET /api/v1/audit?action=access.review: %+v, want the two reviews, newest first", list.Events)
	}
	wantJSON(t, "the first review in the audit log", list.Events[1], mustJSON(t, first))
	if !strings.Contains(string(list.Events[0].Details), `"scope":"all"`) {
		t.Errorf("a review without a scope: %s, want all", list.Events[0].Details)
	}
	if after := len(auditOf(t, f.st, "")); after != before+2 {
		t.Errorf("two reviews wrote %d audit rows", after-before)
	}
}

// TestAccessReviewBounds: a field out of its bounds is 400 naming it, and writes no row: the
// sha256 missing, with sha256:, of 63 or 65 digits, in capitals; generatedAt missing, not RFC
// 3339, or more than 60 seconds after the server's time; a scope that is none; a note empty, of
// 4097 bytes, or with a control character but a line feed; a changeReason, which a review does
// not take. A note of 4096 bytes and a generatedAt 30 seconds ahead are taken.
func TestAccessReviewBounds(t *testing.T) {
	f := newFixture(t, Options{})
	hexSum := strings.Repeat("ab", 32)
	now := time.Now().UTC()
	body := func(fields map[string]any) string {
		b := map[string]any{"sha256": hexSum, "generatedAt": now.Format(time.RFC3339), "note": "ok"}
		for k, v := range fields {
			if v == nil {
				delete(b, k)
			} else {
				b[k] = v
			}
		}
		return mustJSON(t, b)
	}
	for _, tt := range []struct {
		fields map[string]any
		msg    string
	}{
		{map[string]any{"sha256": nil}, "sha256: 0 characters; want the sha256 of the document's bytes, 64 lowercase hexadecimal digits"},
		{map[string]any{"sha256": "sha256:" + hexSum}, "sha256: the 64 hexadecimal digits alone, without sha256:"},
		{map[string]any{"sha256": hexSum[1:]}, "sha256: 63 characters"},
		{map[string]any{"sha256": hexSum + "a"}, "sha256: 65 characters"},
		{map[string]any{"sha256": strings.ToUpper(hexSum)}, "sha256: 64 characters; want"},
		{map[string]any{"sha256": strings.Repeat("g", 64)}, "sha256: 64 characters; want"},
		{map[string]any{"generatedAt": nil}, "generatedAt: required"},
		{map[string]any{"generatedAt": "yesterday"}, `generatedAt: "yesterday" is not an RFC 3339 time`},
		{map[string]any{"generatedAt": now.Add(2 * time.Minute).Format(time.RFC3339)}, "generatedAt: " +
			now.Add(2*time.Minute).Format(time.RFC3339) + " is after the server's time"},
		{map[string]any{"scope": "everyone"}, `scope: "everyone" is not all, people or systems`},
		{map[string]any{"scope": ""}, `scope: "" is not all, people or systems`},
		{map[string]any{"note": nil}, "note: required"},
		{map[string]any{"note": strings.Repeat("é", 2049)}, "note: 4098 bytes, at most 4096"},
		{map[string]any{"note": "a\tb"}, "note: a control character"},
		{map[string]any{"note": "a\r\nb"}, "note: a control character"},
		{map[string]any{"note": "a\u0007"}, "note: a control character"},
		{map[string]any{"changeReason": "CHG-1"}, `unknown field "changeReason"`},
		{map[string]any{"sha256": 7}, "sha256: must not be a JSON number"},
	} {
		wantError(t, f.do(http.MethodPost, "/api/v1/access-reviews", body(tt.fields), admin), http.StatusBadRequest, codeInvalid, tt.msg)
	}
	if rows := auditOf(t, f.st, auth.ActionAccessReview); len(rows) != 0 {
		t.Errorf("refused reviews wrote %d rows", len(rows))
	}
	for _, fields := range []map[string]any{
		{"note": strings.Repeat("a\n", 2048)},
		{"generatedAt": now.Add(30 * time.Second).Format(time.RFC3339)},
		{"generatedAt": now.Add(-400 * 24 * time.Hour).Format(time.RFC3339Nano), "scope": "systems"},
	} {
		wantStatus(t, f.do(http.MethodPost, "/api/v1/access-reviews", body(fields), admin), http.StatusCreated)
	}
}

// TestAccessReviewAdminOnly: only the admin role records a review, checked before the body is
// read: a reader and a maintainer on */* are 403 whatever the body; under the admin mode an
// admin's password is 403 and a sign-in without a second factor 401, and a sign-in with one
// records it, the row with the sign-in in its details.
func TestAccessReviewAdminOnly(t *testing.T) {
	f := rbacFixture(t, [3]string{"root", auth.RoleAdmin, "*/*"}, [3]string{"ops", auth.RoleMaintainer, "*/*"},
		[3]string{"alice", auth.RoleReader, "raw/*"})
	body := `{"sha256": "` + strings.Repeat("0", 64) + `", "generatedAt": "2026-01-02T12:00:00Z", "note": "ok"}`
	for _, p := range []string{"ops", "alice"} {
		for _, b := range []string{body, "not json"} {
			wantError(t, f.do(http.MethodPost, "/api/v1/access-reviews", b, auth.Principal{Kind: auth.KindUser, Name: p}),
				http.StatusForbidden, codeForbidden)
		}
	}
	wantStatus(t, f.do(http.MethodPost, "/api/v1/access-reviews", body, auth.Principal{Kind: auth.KindUser, Name: "root"}),
		http.StatusCreated)

	m := adminModeFixture(t)
	root := auth.Principal{Kind: auth.KindUser, Name: "root"}
	wantError(t, m.doAs(http.MethodPost, "/api/v1/access-reviews", body, root, auth.Credential{Kind: auth.CredPassword}),
		http.StatusForbidden, codeForbidden, "auth.admin.requireMFA")
	oneFactor := auth.Credential{Kind: auth.CredPAT, TokenID: 6, SignIn: &auth.SignIn{At: time.Now(), AMR: []string{"pwd"}}}
	wantError(t, m.doAs(http.MethodPost, "/api/v1/access-reviews", body, root, oneFactor), http.StatusUnauthorized, codeUnauthenticated)
	mfa := auth.Credential{Kind: auth.CredPAT, TokenID: 5, SignIn: &auth.SignIn{AMR: []string{"pwd", "otp"}}}
	var row AuditEntry
	decodeJSON(t, m.doAs(http.MethodPost, "/api/v1/access-reviews", body, root, mfa), http.StatusCreated, &row)
	if !strings.Contains(string(row.Details), `"signIn":{"amr":["pwd","otp"],"token":5}`) {
		t.Errorf("the review of a web UI's sign-in: details %s, want its signIn", row.Details)
	}
	if rows := auditOf(t, m.st, auth.ActionAccessReview); len(rows) != 1 {
		t.Errorf("%d rows, want the one review with a second factor", len(rows))
	}
}
