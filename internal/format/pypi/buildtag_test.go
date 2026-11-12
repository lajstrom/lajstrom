package pypi

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestBuildTag is #726 for Build and Validate: a wheel with a build tag has the variant
// <build>-<tag set>, and so a tag of its own beside the wheel of the same version and tags
// without one; the tag keeps to the OCI grammar, a long one hashed. Validate reads the variant
// without the build tag too, the form uploads wrote before #726, which Build writes when the
// upload's ParamVariant names it, and no other value of it.
func TestBuildTag(t *testing.T) {
	long := strings.Repeat("manylinux_2_17_x86_64.", 5) + "manylinux2014_x86_64"
	for _, tt := range []struct {
		build, platform, variant, tag string
	}{
		{"1", "", "1-py3-none-any", "1.1.0__1-py3-none-any"},
		{"20251112a", "", "20251112a-py3-none-any", "1.1.0__20251112a-py3-none-any"},
		{"2", long, "2-py3-none-" + long, ""}, // longer than 128: hashed
	} {
		whl, wi := testutil.Wheel(testutil.WheelOptions{Name: "Foo.Bar", Version: "1.1.0", Build: tt.build, PlatformTag: tt.platform})
		built := buildChecked(t, wi.Filename, whl, nil, tt.variant)
		if (tt.tag != "" && built.Tag != tt.tag) || !ociTag.MatchString(built.Tag) {
			t.Errorf("%s: tag %q", wi.Filename, built.Tag)
		}
		tagSet := strings.TrimPrefix(tt.variant, tt.build+"-")
		plain, pi := testutil.Wheel(testutil.WheelOptions{Name: "Foo.Bar", Version: "1.1.0", PlatformTag: tt.platform})
		if other := buildChecked(t, pi.Filename, plain, map[string]string{ParamVariant: "py2-none-any"}, tagSet); other.Tag == built.Tag {
			t.Errorf("%s and %s have one tag %q", wi.Filename, pi.Filename, built.Tag)
		}
		for param, want := range map[string]string{tagSet: tagSet, "3-" + tagSet: tt.variant, VariantSdist: tt.variant} {
			buildChecked(t, wi.Filename, whl, map[string]string{ParamVariant: param}, want)
		}
		for _, wrong := range []string{"3-" + tagSet, "py2-none-any", VariantSdist} {
			built.Manifest.Annotations[profile.AnnotationVariant] = wrong
			if _, err := (Plugin{}).Validate(t.Context(), repo, built.Manifest, fetcher(t, built)); !errors.Is(err, format.ErrInvalid) {
				t.Errorf("%s with the variant %q: Validate %v, want ErrInvalid", wi.Filename, wrong, err)
			}
		}
	}
}

// buildChecked builds the upload of filename with the params added and checks its variant, in
// the annotations and the entry, the tag of version 1.1.0 with it, and that Validate takes the
// manifest.
func buildChecked(t *testing.T, filename string, b []byte, params map[string]string, variant string) *format.Built {
	t.Helper()
	up := upload(filename, b)
	maps.Copy(up.Params, params)
	built, err := Plugin{}.Build(t.Context(), repo, up)
	must(t, err)
	if got := built.Manifest.Annotations[profile.AnnotationVariant]; got != variant || built.Entry.Variant != variant ||
		built.Tag != profile.Tag("1.1.0", variant) {
		t.Errorf("%s with %v: variant %q, entry %q, tag %q, want %q", filename, params, got, built.Entry.Variant, built.Tag, variant)
	}
	if _, err := (Plugin{}).Validate(t.Context(), repo, built.Manifest, fetcher(t, built)); err != nil {
		t.Errorf("%s with %v: Validate: %v", filename, params, err)
	}
	return built
}

// TestUploadBuildTag is #726 through twine's upload: the wheel of foo.bar 1.1.0 and the same
// version's wheel with the build tag 1 are both stored and both on the page (pip prefers the
// higher build), and each downloads its bytes; the same bytes again are 200, other bytes under
// the file name with the build tag are 409 naming it.
func TestUploadBuildTag(t *testing.T) {
	e := newEnv(t)
	plain, pi := testutil.Wheel(testutil.WheelOptions{Name: "Foo.Bar", Version: "1.1.0"})
	rebuilt, bi := testutil.Wheel(testutil.WheelOptions{Name: "Foo.Bar", Version: "1.1.0", Build: "1", Summary: "fixed"})
	for _, f := range []form{twineForm(pi.Filename, "Foo.Bar", "1.1.0", PackagetypeWheel, plain), twineForm(bi.Filename, "Foo.Bar", "1.1.0", PackagetypeWheel, rebuilt)} {
		if rr := e.post(t, f); rr.Code != http.StatusOK {
			t.Fatalf("upload %s: %d %s", f.filename, rr.Code, rr.Body)
		}
	}
	e.wantFiles(t, "foo-bar", pi.Filename, bi.Filename)
	for name, b := range map[string][]byte{pi.Filename: plain, bi.Filename: rebuilt} {
		if rr := e.do(http.MethodGet, "files/foo-bar/1.1.0/"+name); rr.Code != http.StatusOK || rr.Body.String() != string(b) {
			t.Errorf("GET %s: %d, %d bytes", name, rr.Code, rr.Body.Len())
		}
	}
	if rr := e.post(t, twineForm(bi.Filename, "Foo.Bar", "1.1.0", PackagetypeWheel, rebuilt)); rr.Code != http.StatusOK {
		t.Errorf("the same bytes again: %d %s", rr.Code, rr.Body)
	}
	other, _ := testutil.Wheel(testutil.WheelOptions{Name: "Foo.Bar", Version: "1.1.0", Build: "1", Summary: "other"})
	if rr := e.post(t, twineForm(bi.Filename, "Foo.Bar", "1.1.0", PackagetypeWheel, other)); rr.Code != http.StatusConflict ||
		!strings.Contains(rr.Body.String(), "File already exists: "+bi.Filename+" is in the repository with other bytes") {
		t.Errorf("other bytes under %s: %d %s", bi.Filename, rr.Code, rr.Body)
	}
	e.wantFiles(t, "foo-bar", pi.Filename, bi.Filename)
}

// TestUploadEarlierBuildTag is #726 for a wheel with a build tag that an upload stored before
// it, under the variant without the build tag: the same file again is the idempotent repeat at
// that tag, not a second copy; other bytes under its name are 409 naming it, and so is the
// wheel without a build tag, which has that tag, with a message that names the file there; a
// wheel of another build tag is stored beside it, in the new form.
func TestUploadEarlierBuildTag(t *testing.T) {
	e := newEnv(t)
	legacy, li := testutil.Wheel(testutil.WheelOptions{Name: "Foo.Bar", Version: "1.1.0", Build: "1"})
	up := upload(li.Filename, legacy)
	up.Params[ParamVariant] = "py3-none-any" // what Build wrote before #726
	pub, err := e.pipe.Upload(t.Context(), e.repo, up, auth.Principal{Kind: auth.KindUser, Name: "alice"})
	must(t, err)
	if pub.Entry.Variant != "py3-none-any" {
		t.Fatalf("the legacy wheel has the variant %q", pub.Entry.Variant)
	}
	if rr := e.post(t, twineForm(li.Filename, "Foo.Bar", "1.1.0", PackagetypeWheel, legacy)); rr.Code != http.StatusOK {
		t.Errorf("the same bytes again: %d %s", rr.Code, rr.Body)
	}
	e.wantFiles(t, "foo-bar", li.Filename)
	other, _ := testutil.Wheel(testutil.WheelOptions{Name: "Foo.Bar", Version: "1.1.0", Build: "1", Summary: "other"})
	if rr := e.post(t, twineForm(li.Filename, "Foo.Bar", "1.1.0", PackagetypeWheel, other)); rr.Code != http.StatusConflict ||
		!strings.Contains(rr.Body.String(), "File already exists: "+li.Filename+" is in the repository with other bytes") {
		t.Errorf("other bytes under %s: %d %s", li.Filename, rr.Code, rr.Body)
	}
	plain, pi := testutil.Wheel(testutil.WheelOptions{Name: "Foo.Bar", Version: "1.1.0"})
	if rr := e.post(t, twineForm(pi.Filename, "Foo.Bar", "1.1.0", PackagetypeWheel, plain)); rr.Code != http.StatusConflict ||
		!strings.Contains(rr.Body.String(), "File already exists: "+li.Filename+" is in the repository under the version and tags of "+pi.Filename) {
		t.Errorf("%s beside %s: %d %s", pi.Filename, li.Filename, rr.Code, rr.Body)
	}
	second, si := testutil.Wheel(testutil.WheelOptions{Name: "Foo.Bar", Version: "1.1.0", Build: "2"})
	if rr := e.post(t, twineForm(si.Filename, "Foo.Bar", "1.1.0", PackagetypeWheel, second)); rr.Code != http.StatusOK {
		t.Errorf("%s: %d %s", si.Filename, rr.Code, rr.Body)
	}
	e.wantFiles(t, "foo-bar", li.Filename, si.Filename)
}

// wantFiles checks that the JSON page of project lists the files, each once.
func (e *env) wantFiles(t *testing.T, project string, files ...string) {
	t.Helper()
	rr := e.do(http.MethodGet, "simple/"+project+"/", "Accept", contentTypeJSON)
	var page jsonPage
	must(t, json.Unmarshal(rr.Body.Bytes(), &page))
	var got []string
	for _, f := range page.Files {
		got = append(got, f.Filename)
	}
	slices.Sort(got)
	slices.Sort(files)
	if !slices.Equal(got, files) {
		t.Errorf("the page of %s lists %q, want %q", project, got, files)
	}
}
