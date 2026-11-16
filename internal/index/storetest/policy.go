package storetest

import (
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// generation returns the policy generation of s.
func generation(t *testing.T, s index.Reader) int64 {
	t.Helper()
	gen, err := s.PolicyGeneration(t.Context())
	must(t, err)
	return gen
}

// testPolicyRules: a rule is created, read back whole, listed by id and deleted; each change
// moves the generation, and a rule that does not exist is not found.
func testPolicyRules(t *testing.T, s index.Store) {
	ctx := t.Context()
	g0 := generation(t, s)
	deny := index.PolicyRule{RepoMask: "pypi/proxy-pypi", Name: "requests", Version: "==2.32.0", Variant: "*", Effect: index.PolicyDeny,
		Reason: "CVE-2026-1234", CreatedBy: "user:u1", CreatedAt: at(1)}
	must(t, s.CreatePolicyRule(ctx, &deny))
	allow := index.PolicyRule{RepoMask: "npm/*", Name: "@acme/*", Version: "*", Variant: "*", Effect: index.PolicyAllow,
		Reason: "ours", CreatedBy: "token:3", ExpiresAt: ptr(at(500))}
	must(t, s.CreatePolicyRule(ctx, &allow))
	if deny.ID == 0 || allow.ID <= deny.ID || allow.CreatedAt.IsZero() {
		t.Fatalf("CreatePolicyRule did not fill the rules: %+v, %+v", deny, allow)
	}
	if g := generation(t, s); g != g0+2 {
		t.Errorf("the generation after two rules: %d, want %d", g, g0+2)
	}
	got, err := s.GetPolicyRule(ctx, deny.ID)
	must(t, err)
	same(t, "the rule read back", deny, *got)
	all, err := s.ListPolicyRules(ctx)
	must(t, err)
	same(t, "the rules by id", []index.PolicyRule{deny, allow}, all)
	if !allow.Expired(at(500)) || allow.Expired(at(499)) || deny.Expired(at(1<<30)) {
		t.Error("Expired: a rule expires at its ExpiresAt, and one without never does")
	}

	must(t, s.DeletePolicyRule(ctx, deny.ID))
	if g := generation(t, s); g != g0+3 {
		t.Errorf("the generation after the delete: %d, want %d", g, g0+3)
	}
	_, err = s.GetPolicyRule(ctx, deny.ID)
	wantErr(t, "the deleted rule", err, index.ErrNotFound)
	wantErr(t, "a delete of a rule that is not there", s.DeletePolicyRule(ctx, deny.ID), index.ErrNotFound)
	if g := generation(t, s); g != g0+3 {
		t.Errorf("a delete that found nothing moved the generation to %d", g)
	}
}

// verdictOf is a verdict row of the subject d with verdict and source.
func verdictOf(path string, d string, verdict, source string) *index.Verdict {
	return &index.Verdict{Path: path, Subject: dg(d), Verdict: verdict, Reason: "because", Source: source, SignedBy: "lajstrom"}
}

// testVerdicts: an attachment with a verdict writes its verdicts row with it, listed by repo and
// by subject; replacing the referrer's rows, deleting the referrer and ResetDerived take it,
// and each moves the generation; an attachment without a verdict leaves it alone.
func testVerdicts(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "apt", "internal")
	pkg := newPackage(t, s, repo.ID, "openssl")
	v := newVersion(t, s, pkg.ID, "3.0.2")
	g0 := generation(t, s)

	a := attachment("verdict", "layer v", 10, 100)
	a.Kind, a.PredicateType = index.AttachmentAttestation, "dev.lajstrom.verdict/v1"
	a.Verdict = verdictOf("openssl", string(v.Digest), index.PolicyDeny, "lajstrom:manual")
	a.Verdict.Subject = v.Digest
	a.Verdict.ValidUntil = ptr(at(900))
	rows := []index.Attachment{a, attachment("verdict", "layer plain", 10, 100)}
	must(t, s.ReplaceAttachments(ctx, v.ID, dg("verdict"), rows))
	if g := generation(t, s); g != g0+1 {
		t.Errorf("the generation after a verdict was written: %d, want %d", g, g0+1)
	}
	got, err := s.ListVerdicts(ctx, index.VerdictFilter{RepoID: repo.ID})
	must(t, err)
	want := index.Verdict{ID: got[0].ID, RepoID: repo.ID, AttachmentID: rows[0].ID, Path: "openssl", Subject: v.Digest,
		Referrer: dg("verdict"), Layer: dg("layer v"), Verdict: index.PolicyDeny, Reason: "because", Source: "lajstrom:manual",
		ValidUntil: ptr(at(900)), SignedBy: "lajstrom", CreatedAt: ptr(at(10)), IndexedAt: at(100)}
	same(t, "the verdict of the repo", []index.Verdict{want}, got)
	got, err = s.ListVerdicts(ctx, index.VerdictFilter{Subject: v.Digest})
	must(t, err)
	same(t, "the verdict of the subject", []index.Verdict{want}, got)
	got, err = s.ListVerdicts(ctx, index.VerdictFilter{Subject: dg("another")})
	must(t, err)
	same(t, "another subject", []index.Verdict{}, got)

	// Another referrer without a verdict moves nothing.
	must(t, s.ReplaceAttachments(ctx, v.ID, dg("sbom"), []index.Attachment{attachment("sbom", "layer s", 10, 100)}))
	if g := generation(t, s); g != g0+1 {
		t.Errorf("an attachment without a verdict moved the generation to %d", g)
	}
	// The referrer's rows again, without the verdict: it goes, and the generation moves.
	must(t, s.ReplaceAttachments(ctx, v.ID, dg("verdict"), []index.Attachment{attachment("verdict", "layer plain", 10, 200)}))
	got, err = s.ListVerdicts(ctx, index.VerdictFilter{RepoID: repo.ID})
	must(t, err)
	same(t, "after the referrer lost its verdict", []index.Verdict{}, got)
	if g := generation(t, s); g != g0+2 {
		t.Errorf("the generation after the verdict went: %d, want %d", g, g0+2)
	}

	// The delete of the referrer takes it.
	must(t, s.ReplaceAttachments(ctx, v.ID, dg("verdict"), []index.Attachment{a}))
	n, err := s.DeleteAttachments(ctx, repo.ID, dg("verdict"))
	must(t, err)
	got, err = s.ListVerdicts(ctx, index.VerdictFilter{Subject: v.Digest})
	must(t, err)
	if n != 1 || len(got) != 0 || generation(t, s) != g0+4 {
		t.Errorf("DeleteAttachments: %d rows, %d verdicts left, generation %d; want 1, 0, %d", n, len(got), generation(t, s), g0+4)
	}

	// ResetDerived takes it with the attachments.
	must(t, s.ReplaceAttachments(ctx, v.ID, dg("verdict"), []index.Attachment{a}))
	must(t, s.ResetDerived(ctx, repo.ID))
	got, err = s.ListVerdicts(ctx, index.VerdictFilter{RepoID: repo.ID})
	must(t, err)
	same(t, "after ResetDerived", []index.Verdict{}, got)
}

// testOCIVerdicts: the verdicts of an oci repo's referrer are replaced whole and deleted, touched
// and swept by path; Scoped lists the rows of other repos whose scope is set; the rows of a
// repo go with it.
func testOCIVerdicts(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "oci", "images")
	other := newRepo(t, s, "oci", "other")
	g0 := generation(t, s)

	deny := verdictOf("app", "image", index.PolicyDeny, "trivy")
	deny.IndexedAt = at(100)
	must(t, s.ReplaceOCIVerdicts(ctx, repo.ID, dg("att"), []index.Verdict{*deny}))
	wide := verdictOf("lib", "image", index.PolicyDeny, "lajstrom:manual")
	wide.Scope, wide.IndexedAt = "*/*", at(100)
	must(t, s.ReplaceOCIVerdicts(ctx, other.ID, dg("wide"), []index.Verdict{*wide}))
	if g := generation(t, s); g != g0+2 {
		t.Errorf("the generation after two referrers: %d, want %d", g, g0+2)
	}
	got, err := s.ListVerdicts(ctx, index.VerdictFilter{RepoID: repo.ID})
	must(t, err)
	if len(got) != 1 || got[0].Referrer != dg("att") || got[0].RepoID != repo.ID || got[0].AttachmentID != 0 || got[0].Path != "app" {
		t.Fatalf("the verdicts of the repo: %+v", got)
	}
	got, err = s.ListVerdicts(ctx, index.VerdictFilter{RepoID: repo.ID, Scoped: true})
	must(t, err)
	if len(got) != 2 {
		t.Errorf("with the scoped rows: %d verdicts, want the repo's and the other repo's of scope */*", len(got))
	}

	// Touch one, sweep the path: the row not seen goes.
	second := verdictOf("app", "image2", index.PolicyQuarantine, "trivy")
	second.IndexedAt = at(100)
	must(t, s.ReplaceOCIVerdicts(ctx, repo.ID, dg("att2"), []index.Verdict{*second}))
	got, err = s.ListVerdicts(ctx, index.VerdictFilter{RepoID: repo.ID, Subject: dg("image")})
	must(t, err)
	must(t, s.TouchOCIVerdicts(ctx, []int64{got[0].ID}, at(300)))
	n, err := s.SweepOCIVerdicts(ctx, repo.ID, "app", at(200))
	must(t, err)
	got, err = s.ListVerdicts(ctx, index.VerdictFilter{RepoID: repo.ID})
	must(t, err)
	if n != 1 || len(got) != 1 || got[0].Subject != dg("image") {
		t.Errorf("the sweep: %d removed, %+v left; want the one not touched gone", n, got)
	}
	if n, err := s.SweepOCIVerdicts(ctx, repo.ID, "elsewhere", at(1000)); err != nil || n != 0 {
		t.Errorf("a sweep of another path: %d, %v", n, err)
	}

	ociVerdictsDelete(t, s, repo, other)
}

// ociVerdictsDelete: the delete of a referrer's verdicts, and of a repo, take them; a delete that
// finds nothing moves nothing.
func ociVerdictsDelete(t *testing.T, s index.Store, repo, other index.Repo) {
	ctx := t.Context()
	n, err := s.DeleteOCIVerdicts(ctx, repo.ID, dg("att"))
	must(t, err)
	if n != 1 {
		t.Errorf("DeleteOCIVerdicts: %d rows, want 1", n)
	}
	gen := generation(t, s)
	if n, err := s.DeleteOCIVerdicts(ctx, repo.ID, dg("att")); err != nil || n != 0 || generation(t, s) != gen {
		t.Errorf("a delete that found nothing: %d, %v, and the generation moved: %v", n, err, generation(t, s) != gen)
	}
	must(t, s.DeleteRepo(ctx, other.ID))
	got, err := s.ListVerdicts(ctx, index.VerdictFilter{Subject: dg("image")})
	must(t, err)
	same(t, "after the other repo was deleted", []index.Verdict{}, got)
}

// testPolicySeen: the first time a version is seen is kept, a later put reads it back.
func testPolicySeen(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "maven", "central")
	_, err := s.GetPolicySeen(ctx, repo.ID, "com.acme:lib", "1.0", "")
	wantErr(t, "a version never seen", err, index.ErrNotFound)
	first := index.PolicySeen{RepoID: repo.ID, Name: "com.acme:lib", Version: "1.0", SeenAt: at(10)}
	must(t, s.PutPolicySeen(ctx, &first))
	again := index.PolicySeen{RepoID: repo.ID, Name: "com.acme:lib", Version: "1.0", SeenAt: at(99)}
	must(t, s.PutPolicySeen(ctx, &again))
	same(t, "the second put reads the first time", at(10), again.SeenAt)
	got, err := s.GetPolicySeen(ctx, repo.ID, "com.acme:lib", "1.0", "")
	must(t, err)
	same(t, "the row", first, *got)
}

// testIndexCacheExpiry: a document's ExpiresAt is stored and read back, and a document stored
// again without one has none.
func testIndexCacheExpiry(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo := newRepo(t, s, "pypi", "proxy")
	gen, err := s.IndexCacheGeneration(ctx, repo.ID)
	must(t, err)
	c := &index.Cache{RepoID: repo.ID, Scope: "simple/requests", Kind: "simple.html", Content: []byte("<html/>"), SHA256: "x",
		RenderedAt: at(1), ExpiresAt: ptr(at(60))}
	if _, err := s.PutIndexCache(ctx, c, gen); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetIndexCache(ctx, repo.ID, "simple/requests", "simple.html")
	must(t, err)
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(at(60)) || !got.Expired(at(60)) || got.Expired(at(59)) {
		t.Errorf("ExpiresAt read back: %v", got.ExpiresAt)
	}
	c.ExpiresAt = nil
	if _, err := s.PutIndexCache(ctx, c, gen); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetIndexCache(ctx, repo.ID, "simple/requests", "simple.html")
	must(t, err)
	if got.ExpiresAt != nil || got.Expired(at(1<<30)) {
		t.Errorf("a document stored again without an expiry has %v", got.ExpiresAt)
	}
}
