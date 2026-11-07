package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/quick"
)

var (
	// escapedGrammar is what Escape produces for a non-empty input (§4.3).
	escapedGrammar = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	// ociTagGrammar is the OCI distribution tag grammar.
	ociTagGrammar = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	// componentGrammar is the OCI distribution path-component grammar.
	componentGrammar = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)
)

// hashedComponent is the §4.3 fallback: "h0__" + first 32 hex digits of sha256(s).
func hashedComponent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "h0__" + hex.EncodeToString(sum[:])[:32]
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
		{"a--b", "a--b"},
		// §4.3, step 3: the hash fallback.
		{"_private", hashedComponent("_private")},
		{"-x", hashedComponent("-x")},
		{".hidden", hashedComponent(".hidden")},
		{"Пакет", hashedComponent("Пакет")},
		{"a.-b", hashedComponent("a.-b")},
		{"a..b", hashedComponent("a..b")},
		{"trailing.", hashedComponent("trailing.")},
		{"", hashedComponent("")},
	} {
		got := EscapeComponent(tc.in)
		if got != tc.want {
			t.Errorf("EscapeComponent(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !componentGrammar.MatchString(got) {
			t.Errorf("EscapeComponent(%q) = %q does not match the component grammar", tc.in, got)
		}
	}
	// The hash input is the original spelling, not the lower-cased one.
	if EscapeComponent("_A") == EscapeComponent("_a") {
		t.Error(`EscapeComponent("_A") == EscapeComponent("_a"), want sha256 of the original`)
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
	c := EscapeComponent(s)
	if !componentGrammar.MatchString(c) {
		t.Fatalf("EscapeComponent(%q) = %q does not match the component grammar", s, c)
	}
	if u, err := Unescape(c); c != hashedComponent(s) && (err != nil || u != lowerASCII(s)) {
		t.Fatalf("EscapeComponent(%q) = %q is neither Escape(lower(s)) nor the hash form", s, c)
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

func FuzzEscape(f *testing.F) {
	for _, s := range []string{
		"", "_", ":", "+", "~", "/", " ", ".hidden", "-x", "1:1.18.0-6ubuntu14.4", "manylinux_2_17",
		"Docs", "Readme.MD", "libstdc++6", "Lib-Core", "com.example", "Пакет", "_h00", "h0_ab", "_2e",
		strings.Repeat("~", 43),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		checkEscape(t, s)
		if u, err := Unescape(s); err == nil && Escape(u) != s {
			t.Fatalf("Unescape(%q) = %q, but Escape of it is %q", s, u, Escape(u))
		}
		if s != "" && !ociTagGrammar.MatchString(Tag(s, s)) {
			t.Fatalf("Tag(%q, %q) = %q does not match the OCI tag grammar", s, s, Tag(s, s))
		}
	})
}
