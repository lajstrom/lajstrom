package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

var (
	// escapedGrammar is what Escape produces for a non-empty input (§4.3).
	escapedGrammar = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	// ociTagGrammar is the OCI distribution tag grammar.
	ociTagGrammar = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	// specComponentGrammar is the path-component grammar of the OCI distribution specification 1.1.
	specComponentGrammar = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)
	// componentGrammar is the grammar of ADR 0017, the 1.0 grammar that Gitea and Forgejo check.
	componentGrammar = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
)

// hashedComponent is the §4.3 fallback: "h0_h" + first 32 hex digits of sha256(s).
func hashedComponent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "h0_h" + hex.EncodeToString(sum[:])[:32]
}

// v1Component is EscapeComponent before ADR 0017, written out: Escape of the lower-cased name,
// or "h0__" and 32 hex digits when that is not of the 1.1 grammar.
func v1Component(s string) string {
	if e := Escape(lowerASCII(s)); specComponentGrammar.MatchString(e) {
		return e
	}
	sum := sha256.Sum256([]byte(s))
	return "h0__" + hex.EncodeToString(sum[:])[:32]
}

// checkComponent fails unless c keeps to ADR 0017: the 1.0 grammar, and no reserved word.
func checkComponent(t *testing.T, s, c string) {
	t.Helper()
	if !componentGrammar.MatchString(c) {
		t.Fatalf("EscapeComponent(%q) = %q does not match the 1.0 component grammar", s, c)
	}
	for _, w := range []string{"blobs", ".uploads", ".sync"} {
		if strings.HasSuffix(c, w) {
			t.Fatalf("EscapeComponent(%q) = %q ends in %q", s, c, w)
		}
	}
	if slices.Contains([]string{"index.json", "oci-layout", "manifests", "referrers"}, c) {
		t.Fatalf("EscapeComponent(%q) = %q", s, c)
	}
}

// hashedTag is the §4.2 fallback: "_h" + first 40 hex digits of sha256 of the unhashed tag.
func hashedTag(version, variant string) string {
	t := Escape(version)
	if variant != "" {
		t += "__" + Escape(variant)
	}
	sum := sha256.Sum256([]byte(t))
	return "_h" + hex.EncodeToString(sum[:])[:40]
}

func TestEscape(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// §4.3, rule 2.
		{"_", "_5f"},
		{":", "_3a"},
		{"+", "_2b"},
		{"~", "_7e"},
		{"/", "_2f"},
		{" ", "_20"},
		// §4.3, rule 3: a leading '.' or '-' only.
		{".hidden", "_2ehidden"},
		{"-x", "_2dx"},
		{"..", "_2e."},
		{"a.-b", "a.-b"},
		// rule 1, and bytes outside ASCII.
		{"", ""},
		{"AZaz09.-", "AZaz09.-"},
		{"A_Z", "A_5fZ"},
		{"é", "_c3_a9"},
		{"\x00\xff", "_00_ff"},
		{"1:1.18.0-6ubuntu14.4", "1_3a1.18.0-6ubuntu14.4"},
	} {
		if got := Escape(tc.in); got != tc.want {
			t.Errorf("Escape(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got, err := Unescape(tc.want); err != nil || got != tc.in {
			t.Errorf("Unescape(%q) = %q, %v, want %q", tc.want, got, err, tc.in)
		}
	}
}

func TestUnescapeErrors(t *testing.T) {
	for _, in := range []string{
		"_",                            // truncated
		"a_5",                          // truncated
		"_5F",                          // uppercase hex
		"_zz",                          // not hex
		"a__5f",                        // '_' not followed by hex: the tag separator
		"_h" + strings.Repeat("0", 40), // hashed tag
		"_hx",                          // hashed-tag prefix, whatever follows
		"a:b",                          // a byte Escape never writes
		".a",                           // leading '.'
		"-a",                           // leading '-'
		"_41",                          // escape of 'A', which Escape copies
		"a_2e",                         // escape of '.' after the first byte
	} {
		got, err := Unescape(in)
		if !errors.Is(err, ErrInvalidEscape) || !errors.Is(err, ErrInvalid) {
			t.Errorf("Unescape(%q) = %q, %v, want ErrInvalidEscape", in, got, err)
		}
	}
}

func TestEscapeComponent(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// §4.1.
		{"Docs", "docs"},
		{"Readme.MD", "readme.md"},
		{"Requests", "requests"},
		{"libstdc++6", "libstdc_2b_2b6"},
		{"Lib-Core", "lib-core"},
		{"com.example", "com.example"},
		{"a_b", "a_5fb"},
		{"h0__abc", "h0_5f_5fabc"},
		// §4.3, step 3: the hash fallback, "h0_h" since ADR 0017.
		{"_private", hashedComponent("_private")},
		{"_config.yml", hashedComponent("_config.yml")},
		{".gitignore", hashedComponent(".gitignore")},
		{"-x", hashedComponent("-x")},
		{"Пакет", hashedComponent("Пакет")},
		{"a.-b", hashedComponent("a.-b")},
		{"a..b", hashedComponent("a..b")},
		{"a--.b", hashedComponent("a--.b")},
		{"trailing.", hashedComponent("trailing.")},
		{".uploads", hashedComponent(".uploads")},
		{"", hashedComponent("")},
		// Step 4: a run of dashes.
		{"a--b", "a_2d_2db"},
		{"A---B-c", "a_2d_2d_2db-c"},
		{"x-y--z", "x-y_2d_2dz"},
		// Step 5: the reserved words.
		{"blobs", "blob_73"},
		{"Blobs", "blob_73"},
		{"myblobs", "myblob_73"},
		{"netlify-blobs", "netlify-blob_73"},
		{"a--blobs", "a_2d_2dblob_73"},
		{"my.uploads", "my.upload_73"},
		{"x.sync", "x.syn_63"},
		{"index.json", "index.jso_6e"},
		{"oci-layout", "oci-layou_74"},
		// The amendment: Harbor's route words.
		{"manifests", "manifest_73"},
		{"Referrers", "referrer_73"},
		{"manifest", "manifest"},
		{"manifests.yaml", "manifests.yaml"},
		{"k8s-manifests", "k8s-manifests"},
		{"tags", "tags"},
		{"list", "list"},
		// Not reserved.
		{"blob", "blob"},
		{"blobsx", "blobsx"},
		{"uploads", "uploads"},
		{"sync", "sync"},
		{"x_2esync", "x_5f2esync"},
		{"index.jsonl", "index.jsonl"},
		{"sha256", "sha256"},
	} {
		got := EscapeComponent(tc.in)
		if got != tc.want {
			t.Errorf("EscapeComponent(%q) = %q, want %q", tc.in, got, tc.want)
		}
		checkComponent(t, tc.in, got)
	}
	// The hash input is the original spelling, not the lower-cased one.
	if EscapeComponent("_A") == EscapeComponent("_a") {
		t.Error(`EscapeComponent("_A") == EscapeComponent("_a"), want sha256 of the original`)
	}
}

func TestEscapeComponentMax(t *testing.T) {
	long := strings.Repeat("a", 64)
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{long, 64, long},
		{long + "b", 64, hashedComponent(long + "b")},
		{long + "b", 0, long + "b"},
		{strings.Repeat("~", 22), 64, hashedComponent(strings.Repeat("~", 22))}, // 66 bytes escaped
		// The limit applies before the escapes of ADR 0017, as the hash did before it.
		{strings.Repeat("a", 57) + "--blobs", 64, strings.Repeat("a", 57) + "_2d_2dblob_73"},
		{".x", 64, hashedComponent(".x")},
	} {
		if got := EscapeComponentMax(tc.in, tc.max); got != tc.want {
			t.Errorf("EscapeComponentMax(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

func TestReservedComponent(t *testing.T) {
	for c, want := range map[string]bool{
		"blobs": true, "myblobs": true, "x.uploads": true, ".uploads": true, "a.sync": true, "index.json": true,
		"oci-layout": true, "manifests": true, "referrers": true, "manifest": false, "tags": false, "blob": false,
		"uploads": false, "xsync": false, "index.json5": false, "oci-layouts": false,
		"blob_73": false, "h0_h0123": false,
	} {
		if got := ReservedComponent(c); got != want {
			t.Errorf("ReservedComponent(%q) = %v", c, got)
		}
	}
}

// TestLegacyPackagePath: every kind of name that ADR 0017 changes, with the component it had
// before. Reads accept the earlier form beside the new one; a path in it maps to itself.
func TestLegacyPackagePath(t *testing.T) {
	for _, tc := range []struct{ name, now, before string }{
		{".gitignore", hashedComponent(".gitignore"), "h0__" + hashedComponent(".gitignore")[4:]},
		{"_config.yml", hashedComponent("_config.yml"), "h0__" + hashedComponent("_config.yml")[4:]},
		{"a--b", "a_2d_2db", "a--b"},
		{"blobs", "blob_73", "blobs"},
		{"a--blobs", "a_2d_2dblob_73", "a--blobs"},
		{"my.uploads", "my.upload_73", "my.uploads"},
		{"x.sync", "x.syn_63", "x.sync"},
		{"index.json", "index.jso_6e", "index.json"},
		{"oci-layout", "oci-layou_74", "oci-layout"},
		{"libstdc++6", "libstdc_2b_2b6", "libstdc_2b_2b6"},
		{"docs", "docs", "docs"},
	} {
		if got := EscapeComponent(tc.name); got != tc.now {
			t.Errorf("EscapeComponent(%q) = %q, want %q", tc.name, got, tc.now)
		}
		if got := v1Component(tc.name); got != tc.before {
			t.Errorf("v1Component(%q) = %q, want %q", tc.name, got, tc.before)
		}
		for _, p := range []string{tc.now, tc.before} {
			if got := LegacyPackagePath(p); got != tc.before {
				t.Errorf("LegacyPackagePath(%q) = %q, want %q", p, got, tc.before)
			}
		}
	}
	// A path, component by component.
	now := "netlify/" + EscapeComponent("blobs") + "/" + EscapeComponent(".gitignore")
	if got, want := LegacyPackagePath(now), "netlify/blobs/"+v1Component(".gitignore"); got != want {
		t.Errorf("LegacyPackagePath(%q) = %q, want %q", now, got, want)
	}
	// Strings that no writer produces stay as they are.
	for _, p := range []string{"a_2db", "blo_62s", "_2d_2d", "blob_73x", "h0_h0123", "h0_h" + strings.Repeat("g", 32), "a_5f"} {
		if got := LegacyPackagePath(p); got != p {
			t.Errorf("LegacyPackagePath(%q) = %q, want it unchanged", p, got)
		}
	}
}

// TestUnescapeComponent: a name decodes the same from the component it has now and from the
// one it had before ADR 0017, and a hashed component of either form does not decode.
func TestUnescapeComponent(t *testing.T) {
	for _, name := range []string{"docs", "libstdc++6", "a_b", "a--b", "A---B-c", "blobs", "Blobs", "a--blobs", "my.uploads",
		"x.sync", "index.json", "oci-layout", "h0__abc", "com.example"} {
		for _, c := range []string{EscapeComponent(name), v1Component(name)} {
			if got, err := UnescapeComponent(c); err != nil || got != lowerASCII(name) {
				t.Errorf("UnescapeComponent(%q) = %q, %v, want %q", c, got, err, lowerASCII(name))
			}
		}
	}
	for _, c := range []string{
		hashedComponent(".gitignore"), v1Component(".gitignore"), // hashed, in both forms
		"_2ex",                       // Escape(".x"), which is hashed
		"a_2db", "blo_62s", "a-_2db", // escapes that no writer writes
		"Docs", "a:b", "_5f", "", "a__b",
	} {
		if got, err := UnescapeComponent(c); !errors.Is(err, ErrInvalidEscape) {
			t.Errorf("UnescapeComponent(%q) = %q, %v, want ErrInvalidEscape", c, got, err)
		}
	}
	for c, want := range map[string]bool{
		hashedComponent("x"): true, v1Component(".x"): true, "h0_h0": false, strings.ToUpper(hashedComponent("x")): false,
		"h0_x" + hashedComponent("x")[4:]: false,
	} {
		if IsHashedComponent(c) != want {
			t.Errorf("IsHashedComponent(%q) = %v", c, !want)
		}
	}
}

// TestEscapeComponentInjective: over every string of up to 5 bytes of an alphabet that meets
// the rules of ADR 0017, no component, of either form, belongs to two names. Names that differ
// only in case share their component unless it is hashed, whose input is the original spelling.
func TestEscapeComponentInjective(t *testing.T) {
	const alphabet = "a-._Bs"
	seen := map[string]string{}
	add := func(c, name string) {
		if other, ok := seen[c]; ok && other != name {
			t.Errorf("%q is a component of %q and of %q", c, other, name)
		}
		seen[c] = name
	}
	names, level := []string{"blobs", "x.sync", "a.uploads", "index.json", "oci-layout", "a--blobs"}, []string{""}
	for range 5 {
		var next []string
		for _, s := range level {
			for i := range len(alphabet) {
				next = append(next, s+alphabet[i:i+1])
			}
		}
		names, level = append(names, next...), next
	}
	for _, s := range names {
		c, key := EscapeComponent(s), lowerASCII(s)
		if IsHashedComponent(c) {
			key = s
		}
		add(c, key)
		add(v1Component(s), key)
	}
}

func TestTag(t *testing.T) {
	long := strings.Repeat("a", 129)
	for _, tc := range []struct{ version, variant, want string }{
		// §4.2, every row.
		{"1:1.18.0-6ubuntu14.4", "jammy/main/amd64", "1_3a1.18.0-6ubuntu14.4__jammy_2fmain_2famd64"},
		{"2.32.3", "py3-none-any", "2.32.3__py3-none-any"},
		{"2.32.3", "sdist", "2.32.3__sdist"},
		{"1.2.0", "", "1.2.0"},
		{"1.0.0+build.7", "", "1.0.0_2bbuild.7"},
		{"2.1~rc1", "jammy/main/all", "2.1_7erc1__jammy_2fmain_2fall"},
		{"1.0", "manylinux_2_17", "1.0__manylinux_5f2_5f17"},
		// §3: a full wheel tag.
		{"2.32.3", "cp311-cp311-manylinux_2_17_x86_64", "2.32.3__cp311-cp311-manylinux_5f2_5f17_5fx86_5f64"},
		// The separator stays unambiguous where '_' meets it.
		{"a_", "b", "a_5f__b"},
		{"a", "_b", "a___5fb"},
		// 128 characters is the limit; one more is hashed.
		{strings.Repeat("a", 128), "", strings.Repeat("a", 128)},
		{strings.Repeat("a", 63), strings.Repeat("b", 63), strings.Repeat("a", 63) + "__" + strings.Repeat("b", 63)},
		{strings.Repeat("a", 63), strings.Repeat("b", 64), hashedTag(strings.Repeat("a", 63), strings.Repeat("b", 64))},
		{long, "", hashedTag(long, "")},
		{strings.Repeat("~", 43), "", hashedTag(strings.Repeat("~", 43), "")}, // 129 after escaping
	} {
		got := Tag(tc.version, tc.variant)
		if got != tc.want {
			t.Errorf("Tag(%q, %q) = %q, want %q", tc.version, tc.variant, got, tc.want)
		}
		if !ociTagGrammar.MatchString(got) {
			t.Errorf("Tag(%q, %q) = %q does not match the OCI tag grammar", tc.version, tc.variant, got)
		}
	}
}

func TestIsHashedTag(t *testing.T) {
	h := Tag(strings.Repeat("a", 129), "")
	if len(h) != 42 || !strings.HasPrefix(h, "_h") || !IsHashedTag(h) {
		t.Errorf("Tag of 129 characters = %q (len %d), want _h + 40 hex and IsHashedTag", h, len(h))
	}
	for _, tag := range []string{
		"", "1.2.0", "_5f", h[:41], h + "0", "_h" + strings.Repeat("A", 40), "_h" + strings.Repeat("g", 40),
	} {
		if IsHashedTag(tag) {
			t.Errorf("IsHashedTag(%q) = true", tag)
		}
	}
}

func TestRepositoryPath(t *testing.T) {
	for _, tc := range []struct{ prefix, format, repo, pkg, want string }{
		// §4.1 examples.
		{"art/", "apt", "ubuntu-internal", EscapeComponent("nginx"), "art/apt/ubuntu-internal/nginx"},
		{"art/", "pypi", "internal", EscapeComponent("requests"), "art/pypi/internal/requests"},
		{"art/", "maven", "releases", EscapeComponent("com.example") + "/" + EscapeComponent("Lib-Core"),
			"art/maven/releases/com.example/lib-core"},
		{"art/", "raw", "assets", EscapeComponent("Docs") + "/" + EscapeComponent("Readme.MD"),
			"art/raw/assets/docs/readme.md"},
		{"", "raw", "assets", "x", "raw/assets/x"},
	} {
		if got := RepositoryPath(tc.prefix, tc.format, tc.repo, tc.pkg); got != tc.want {
			t.Errorf("RepositoryPath(%q, %q, %q, %q) = %q, want %q", tc.prefix, tc.format, tc.repo, tc.pkg, got, tc.want)
		}
	}
}

// checkEscape asserts the properties of Escape and EscapeComponent for one input. It is shared
// by the property tests and FuzzEscape.
func checkEscape(t *testing.T, s string) {
	t.Helper()
	e := Escape(s)
	if (e == "") != (s == "") || e != "" && !escapedGrammar.MatchString(e) {
		t.Fatalf("Escape(%q) = %q does not match the escaped grammar", s, e)
	}
	if strings.Contains(e, "__") || strings.HasPrefix(e, "_h") {
		t.Fatalf("Escape(%q) = %q contains __ or starts with _h", s, e)
	}
	if u, err := Unescape(e); err != nil || u != s {
		t.Fatalf("Unescape(Escape(%q)) = %q, %v", s, u, err)
	}
	checkEscapeComponent(t, s)
	checkPackagePath(t, s)
}

// checkEscapeComponent asserts the properties of EscapeComponent for one input: the grammar of
// ADR 0017, the round trip, and the form before it, which decodes the same.
func checkEscapeComponent(t *testing.T, s string) {
	t.Helper()
	c, old := EscapeComponent(s), v1Component(s)
	checkComponent(t, s, c)
	switch u, err := UnescapeComponent(c); {
	case c == hashedComponent(s):
		if old != "h0__"+c[4:] {
			t.Fatalf("EscapeComponent(%q) = %q, but %q before ADR 0017", s, c, old)
		}
	case err != nil || u != lowerASCII(s):
		t.Fatalf("UnescapeComponent(EscapeComponent(%q) = %q) = %q, %v: neither the name nor the hash form", s, c, u, err)
	case IsHashedComponent(old):
		t.Fatalf("EscapeComponent(%q) = %q, but %q before ADR 0017", s, c, old)
	default:
		if u, err := UnescapeComponent(old); err != nil || u != lowerASCII(s) {
			t.Fatalf("UnescapeComponent(%q), the form of %q before ADR 0017, = %q, %v", old, s, u, err)
		}
	}
	if LegacyPackagePath(c) != old || LegacyPackagePath(old) != old {
		t.Fatalf("LegacyPackagePath of %q = %q, of %q = %q, want %q", c, LegacyPackagePath(c), old, LegacyPackagePath(old), old)
	}
}

func TestEscapeProperties(t *testing.T) {
	f := func(b []byte) bool {
		checkEscape(t, string(b))
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 20000}); err != nil {
		t.Fatal(err)
	}
}

func TestTagGrammarProperty(t *testing.T) {
	f := func(version, variant []byte) bool {
		return len(version) == 0 || ociTagGrammar.MatchString(Tag(string(version), string(variant)))
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 20000}); err != nil {
		t.Fatal(err)
	}
}

// sampleString draws a string that is likely to stress the separator: short strings over a
// small alphabet with '_', and now and then a long one that Tag has to hash.
func sampleString(r *rand.Rand) string {
	const alphabet = "a_-.5f:~"
	n := r.Intn(7)
	if r.Intn(10) == 0 {
		n = 40 + r.Intn(100)
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

func TestTagInjective(t *testing.T) {
	type pair struct{ version, variant string }
	seen := map[string]pair{}
	add := func(p pair) bool {
		tag := Tag(p.version, p.variant)
		if q, ok := seen[tag]; ok && q != p {
			t.Errorf("Tag(%q, %q) = Tag(%q, %q) = %q", p.version, p.variant, q.version, q.variant, tag)
			return false
		}
		seen[tag] = p
		return true
	}
	// Pairs that differ only in where "__" would fall.
	for _, p := range []pair{{"a_", "b"}, {"a", "_b"}, {"a", "__b"}, {"a__", "b"}, {"a_", "_b"}, {"a", "b"}, {"a__b", ""}, {"", "a__b"}} {
		add(p)
	}
	// Every pair over a small alphabet up to length 3.
	short, level := []string{""}, []string{""}
	for range 3 {
		var next []string
		for _, s := range level {
			for _, c := range "a_.-" {
				next = append(next, s+string(c))
			}
		}
		short, level = append(short, next...), next
	}
	for _, v := range short {
		for _, w := range short {
			add(pair{v, w})
		}
	}
	// A random sample.
	cfg := &quick.Config{
		MaxCount: 50000,
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(sampleString(r))
			args[1] = reflect.ValueOf(sampleString(r))
		},
	}
	if err := quick.Check(func(v, w string) bool { return add(pair{v, w}) }, cfg); err != nil {
		t.Fatal(err)
	}
}

// FuzzEscape checks the properties of Escape, EscapeComponent and, over the input split at '/',
// PackagePath (checkEscape), that Escape copies a string with nothing to escape and makes any
// other at most three times as long, and that Tag of a non-empty string is an OCI tag, hashed
// exactly when the unhashed tag would be longer than 128 characters. testdata/fuzz/FuzzEscape
// has the versions, names, variants and paths of the testutil fixtures, which TestFuzzCorpus
// writes.
func FuzzEscape(f *testing.F) {
	for _, s := range []string{
		"", "_", ":", "+", "~", "/", " ", ".hidden", "-x", "1:1.18.0-6ubuntu14.4", "manylinux_2_17",
		"Docs", "Readme.MD", "libstdc++6", "Lib-Core", "com.example", "Пакет", "_h00", "h0_ab", "_2e",
		strings.Repeat("~", 43), "k8s/manifests/app.yaml", "x/referrers/y", "X/Tags/Listing.TXT", "tags/tags/list",
		"a--b/tags/list.sync", "netlify/blobs",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		checkEscape(t, s)
		e, c := Escape(s), EscapeComponent(s)
		if len(e) > 3*len(s) || len(c) > max(3*len(s), len(hashedComponent(s))) {
			t.Fatalf("%d bytes escape to %d, as a component to %d", len(s), len(e), len(c))
		}
		needs := false
		for i := range len(s) {
			needs = needs || needsEscape(s[i], i)
		}
		if needs == (e == s) {
			t.Fatalf("Escape(%q) = %q, though a byte needs escaping: %v", s, e, needs)
		}
		if s == "" {
			return
		}
		tag := Tag(s, s)
		if !ociTagGrammar.MatchString(tag) {
			t.Fatalf("Tag(%q, %q) = %q does not match the OCI tag grammar", s, s, tag)
		}
		if long := len(e+"__"+e) > maxTagLen; IsHashedTag(tag) != long || long != (tag == hashedTag(s, s)) {
			t.Fatalf("Tag(%q, %q) = %q: hashed %v, %d bytes unhashed", s, s, tag, IsHashedTag(tag), len(e+"__"+e))
		}
	})
}

// FuzzUnescape checks that Unescape accepts exactly what Escape produces. A string it decodes is
// Escape of the result, which is no longer than it. A string it refuses, with ErrInvalidEscape,
// is Escape of nothing: Escape(u) decodes escape by escape to u, so a string that does not
// escape back to itself so decoded is no Escape(u). It checks the same of UnescapeComponent
// and UnescapePackagePath (checkUnescapeComponent, checkUnescapePackagePath), whose other half is
// checkEscape's round trip.
func FuzzUnescape(f *testing.F) {
	for _, s := range []string{"", "a", "_5f", "_2e", "_2E", "_2ex", "_h", "_h00", "a_", "a_5", "_61", "-x", "a-", ".a", "a.",
		"h0__0123", "_2d1", "_5f_5f", "a__b", "_ff_00", "_c3_a9", "a_2d_2db", "blob_73", "blobs", "a--b", "a_2db", "x.syn_63",
		"h0_h0123456789abcdef0123456789abcdef", "h0__0123456789abcdef0123456789abcdef", "a/blob_73/b",
		"k8s/manifest_73/app.yaml", "x/tag_73/list", "x/tags/list", "x/tag_73/other", "manifest_73"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		checkUnescapeComponent(t, s)
		checkUnescapePackagePath(t, s)
		u, err := Unescape(s)
		if err == nil {
			if Escape(u) != s || len(u) > len(s) {
				t.Fatalf("Unescape(%q) = %q, which Escape makes %q", s, u, Escape(u))
			}
			return
		}
		if !errors.Is(err, ErrInvalidEscape) {
			t.Fatalf("Unescape(%q) = %v, want ErrInvalidEscape", s, err)
		}
		if d, ok := decodeEach(s); ok && Escape(d) == s {
			t.Fatalf("Unescape(%q) refused Escape(%q)", s, d)
		}
	})
}

// checkUnescapeComponent checks that UnescapeComponent decodes a component of either form, and
// nothing else: a string it decodes to u is EscapeComponent(u) or the component of u before ADR
// 0017, so no component belongs to two names; any other is ErrInvalidEscape. LegacyPackagePath
// gives a path that it leaves as it is.
func checkUnescapeComponent(t *testing.T, s string) {
	t.Helper()
	u, err := UnescapeComponent(s)
	switch {
	case err == nil && s != EscapeComponent(u) && s != v1Component(u):
		t.Fatalf("UnescapeComponent(%q) = %q, whose components are %q and %q", s, u, EscapeComponent(u), v1Component(u))
	case err != nil && !errors.Is(err, ErrInvalidEscape):
		t.Fatalf("UnescapeComponent(%q) = %v, want ErrInvalidEscape", s, err)
	}
	if l := LegacyPackagePath(s); LegacyPackagePath(l) != l {
		t.Fatalf("LegacyPackagePath(%q) = %q, of which it is %q", s, l, LegacyPackagePath(l))
	}
}

// decodeEach decodes every '_' and the two hex digits after it, in either case, and copies every
// other byte: Unescape without its checks. It fails on a '_' without two hex digits after it.
func decodeEach(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '_' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", false
		}
		c, err := hex.DecodeString(s[i+1 : i+3])
		if err != nil {
			return "", false
		}
		b.WriteByte(c[0])
		i += 2
	}
	return b.String(), true
}
