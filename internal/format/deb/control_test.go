package deb

import (
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
)

// minimal is the smallest control file packageOf accepts.
const minimal = "Package: hello\nVersion: 1.0-1\nArchitecture: amd64\nMaintainer: m <m@example.org>\nDescription: d\n"

// TestPackageOfRefuses checks each rule of the control syntax (Debian Policy §5.1) and of the
// fields the plugin relies on: every case is format.ErrInvalid with its own message.
func TestPackageOfRefuses(t *testing.T) {
	with := func(old, repl string) string { return strings.Replace(minimal, old, repl, 1) }
	for _, tc := range []struct {
		name, control, want string
	}{
		{"empty", "", "control has no fields"},
		{"blank lines only", "\n \n\t\n", "control has no fields"},
		{"not UTF-8", minimal + "X-Bad: \xff\n", "control is not UTF-8"},
		{"control character", with("Description: d", "Description: d\r"), "control line 5 has the control character 0x0d"},
		{"continuation first", " leading continuation\n" + minimal, "control line 1 is a continuation line with no field before it"},
		{"no colon", minimal + "not a field\n", `control line 6 is neither a field nor a continuation line: "not a field"`},
		{"comment", "#Comment: x\n" + minimal, `control line 1: "#Comment" is not a field name`},
		{"hyphen first", "-Field: x\n" + minimal, `control line 1: "-Field" is not a field name`},
		{"space in name", "My Field: x\n" + minimal, `control line 1: "My Field" is not a field name`},
		{"empty name", ": x\n" + minimal, `control line 1: "" is not a field name`},
		{"duplicate", minimal + "VERSION: 2\n", "control line 6: field VERSION is given twice, first as Version"},
		{"second paragraph", minimal + "\nPackage: other\n", "control has a second paragraph at line 7, after the blank line 6"},
		{"no Package", with("Package: hello\n", ""), "control has no Package field"},
		{"no Version", with("Version: 1.0-1\n", ""), "control has no Version field"},
		{"no Architecture", with("Architecture: amd64\n", ""), "control has no Architecture field"},
		{"no Maintainer", with("Maintainer: m <m@example.org>\n", ""), "control has no Maintainer field"},
		{"no Description", with("Description: d\n", ""), "control has no Description field"},
		{"empty Maintainer", with("Maintainer: m <m@example.org>", "Maintainer:   "), "control field Maintainer is empty"},
		{"no synopsis", with("Description: d", "Description:\n extended only"), "control field Description is empty"},
		{"Package uppercase", with("hello", "Hello"), `Package "Hello" is not a package name`},
		{"Package one letter", with("hello", "h"), `Package "h" is not a package name`},
		{"Package underscore", with("hello", "hel_lo"), `Package "hel_lo" is not a package name`},
		{"Package two lines", with("Package: hello", "Package: hello\n world"), "is not a package name"},
		{"Version letter first", with("1.0-1", "v1.0"), `Version "v1.0" is not a Debian version`},
		{"Version colon", with("1.0-1", "1:2:3"), `Version "1:2:3" is not a Debian version`},
		{"Version empty epoch", with("1.0-1", ":1.0"), `Version ":1.0" is not a Debian version`},
		{"Version underscore", with("1.0-1", "1.0_1"), `Version "1.0_1" is not a Debian version`},
		{"Version dash at end", with("1.0-1", "1.0-"), `Version "1.0-" ends with "-"`},
		{"Architecture uppercase", with("amd64", "AMD64"), `Architecture "AMD64" is not an architecture name`},
		{"Architecture two", with("amd64", "amd64 arm64"), `Architecture "amd64 arm64" is not an architecture name`},
		{"Source uppercase", minimal + "Source: Hello\n", `Source "Hello" is not a package name`},
		{"Source unclosed", minimal + "Source: hello (1.0\n", `Source "hello (1.0" is not "name" or "name (version)"`},
		{"Source empty version", minimal + "Source: hello ()\n", `Source "hello ()" is not "name" or "name (version)"`},
		{"Source bad version", minimal + "Source: hello (x1)\n", `the version of Source "x1" is not a Debian version`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkg, err := packageOf([]byte(tc.control))
			if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("packageOf = %+v, %v; want ErrInvalid containing %q", pkg, err, tc.want)
			}
		})
	}
}

// TestPackageOfJoinsErrors checks that the required fields are reported together.
func TestPackageOfJoinsErrors(t *testing.T) {
	_, err := packageOf([]byte("Package: A\nX: y\n"))
	for _, want := range []string{`Package "A"`, "no Version", "no Architecture", "no Maintainer", "no Description"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("packageOf = %v, want it to mention %s", err, want)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for _, v := range []string{"0", "1.0", "1:1.18.0-6ubuntu14.4", "2.1~rc1", "1.0-1-2", "1.0+dfsg-1", "10:1", "1.2.3~beta+git20260101.abc-0ubuntu1~24.04.1"} {
		if err := checkVersion("Version", v); err != nil {
			t.Errorf("checkVersion(%q) = %v", v, err)
		}
	}
	for _, v := range []string{"", "a1.0", "1.0-", "1-2-", ":1", "1:", "1:2:3", "1.0_1", "1.0 1", "1:a", "-1", "1:-1", "1.0/1"} {
		if err := checkVersion("Version", v); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("checkVersion(%q) = %v, want ErrInvalid", v, err)
		}
	}
}

func TestSplitSource(t *testing.T) {
	for _, tc := range []struct{ field, name, version string }{
		{"glibc", "glibc", ""},
		{"hello-src (2.0-1)", "hello-src", "2.0-1"},
		{"hello  (1:2.0)", "hello", "1:2.0"},
	} {
		name, version, err := SplitSource(tc.field)
		if err != nil || name != tc.name || version != tc.version {
			t.Errorf("SplitSource(%q) = %q, %q, %v; want %q, %q", tc.field, name, version, err, tc.name, tc.version)
		}
	}
	for _, field := range []string{"", " glibc", "glibc (", "glibc 2.0", "glibc (2.0) x", "glibc (2.0", "glibc ()"} {
		if _, _, err := SplitSource(field); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("SplitSource(%q) = %v, want ErrInvalid", field, err)
		}
	}
}

func TestParseMD5Sums(t *testing.T) {
	const sum = "d41d8cd98f00b204e9800998ecf8427e"
	got, err := parseMD5Sums([]byte(sum + "  usr/share/a b\n\n" + strings.ToUpper(sum) + " *usr/bin/c\n"))
	if err != nil || len(got) != 2 || got["usr/share/a b"] != sum || got["usr/bin/c"] != sum {
		t.Errorf("parseMD5Sums = %v, %v", got, err)
	}
	for _, tc := range []struct{ md5sums, want string }{
		{sum + "  \n", `md5sums line 1 is not "<md5>  <path>"`},
		{sum + "\tusr/x\n", `md5sums line 1 is not "<md5>  <path>"`},
		{sum + " -usr/x\n", `md5sums line 1 is not "<md5>  <path>"`},
		{"short  usr/x\n", `md5sums line 1 is not "<md5>  <path>"`},
		{strings.Repeat("g", 32) + "  usr/x\n", `md5sums line 1: "gggggggggggggggggggggggggggggggg" is not an md5`},
		{sum + "  usr/\xff\n", "md5sums line 1: the path is not UTF-8"},
		{sum + "  usr/x\n" + sum + "  usr/x\n", `md5sums line 2: "usr/x" is listed twice`},
	} {
		if _, err := parseMD5Sums([]byte(tc.md5sums)); !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseMD5Sums(%q) = %v, want ErrInvalid containing %q", tc.md5sums, err, tc.want)
		}
	}
}
