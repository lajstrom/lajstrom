package deb

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestParseDsc reads the .dsc of testutil, unsigned and signed, a native one and one with an
// epoch: the fields, the name of the .dsc, the files in the order of Checksums-Sha256 with the
// checksums of every list; a signed one says what the unsigned one says, its armor left out.
func TestParseDsc(t *testing.T) {
	for _, opts := range []testutil.DscOptions{
		{}, {Signed: true}, {Version: "2.0"}, {Source: "libfoo", Version: "1:1.2-3", Signed: true},
	} {
		t.Run(fmt.Sprintf("%s %s signed %v", opts.Source, opts.Version, opts.Signed), func(t *testing.T) {
			d := testutil.Dsc(opts)
			got, err := ParseDsc(d.Dsc)
			if err != nil {
				t.Fatal(err)
			}
			if got.Source != d.Source || got.Version != d.Version || got.Format != d.Format || got.Filename != d.Filename {
				t.Errorf("%s %s %s %s, want %s %s %s %s", got.Source, got.Version, got.Format, got.Filename,
					d.Source, d.Version, d.Format, d.Filename)
			}
			var want []SourceFile
			for _, f := range d.Files {
				want = append(want, SourceFile{Name: f.Name, Size: f.Size, MD5: f.MD5, SHA1: f.SHA1, SHA256: f.SHA256})
			}
			if diff := cmp.Diff(want, got.Files); diff != "" {
				t.Errorf("files (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(d.Control, got.Control.Map()); diff != "" {
				t.Errorf("fields (-want +got):\n%s", diff)
			}
		})
	}
}

// TestParseDscArmor: a dash-escaped line of a signed .dsc is unescaped, CRLF line ends are
// read, and the blank lines around a signed message are allowed.
func TestParseDscArmor(t *testing.T) {
	d := testutil.Dsc(testutil.DscOptions{})
	para := strings.Replace(string(d.Paragraph), "Maintainer: ", "Maintainer: -x ", 1)
	signed := "\n-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n" + para + "-----BEGIN PGP SIGNATURE-----\n\nAAAA\n-----END PGP SIGNATURE-----\n\n"
	for name, b := range map[string]string{
		"dash-escaped": strings.Replace(signed, "\nFormat:", "\n- Format:", 1),
		"crlf":         strings.ReplaceAll(signed, "\n", "\r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ParseDsc([]byte(b))
			if err != nil {
				t.Fatal(err)
			}
			if v, _ := got.Control.Get("Maintainer"); !strings.HasPrefix(v, "-x ") || got.Format != d.Format {
				t.Errorf("Maintainer %q, Format %q", v, got.Format)
			}
		})
	}
}

// TestParseDscRefuses: every .dsc that is not what the plugin reads is format.ErrInvalid, and
// says why.
func TestParseDscRefuses(t *testing.T) {
	d := testutil.Dsc(testutil.DscOptions{})
	para := string(d.Paragraph)
	orig, debian := d.Files[0], d.Files[1]
	field := func(name, value string) string { // para with the field name's value replaced, or the field gone
		lines := strings.Split(para, "\n")
		var out []string
		for i := 0; i < len(lines); i++ {
			if !strings.HasPrefix(lines[i], name+":") {
				out = append(out, lines[i])
				continue
			}
			for i+1 < len(lines) && strings.HasPrefix(lines[i+1], " ") {
				i++
			}
			if value != "-" {
				out = append(out, name+":"+value)
			}
		}
		return strings.Join(out, "\n")
	}
	list := func(lines ...string) string { return "\n " + strings.Join(lines, "\n ") }
	sha := func(f testutil.SourceFile) string { return fmt.Sprintf("%s %d %s", f.SHA256, f.Size, f.Name) }
	md5 := func(f testutil.SourceFile) string { return fmt.Sprintf("%s %d %s", f.MD5, f.Size, f.Name) }
	tooMany := make([]string, maxSourceFiles+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("%s %d hello_%d.tar.gz", orig.SHA256, i, i)
	}
	for _, tc := range []struct {
		name, dsc, want string
	}{
		{"no Format", field("Format", "-"), "has no Format field"},
		{"no Source", field("Source", "-"), "has no Source field"},
		{"bad Source", field("Source", " Hello"), `Source "Hello" is not a package name`},
		{"no Version", field("Version", "-"), "has no Version field"},
		{"bad Version", field("Version", " one"), `Version "one" is not a Debian version`},
		{"two lines of Version", field("Version", " 1.0\n 2.0"), "field Version has more than one line"},
		{"no Files", field("Files", "-"), "has no Files field"},
		{"no Checksums-Sha256", field("Checksums-Sha256", "-"), "has no Checksums-Sha256 field"},
		{"a value on the first line", field("Files", " "+md5(orig)), "has a value on its first line"},
		{"two words", field("Files", list(orig.MD5+" "+orig.Name)), `line 1 of the .dsc field Files is not "<checksum> <size> <name>"`},
		{"short md5", field("Files", list("abc 1 hello_1.tar.gz")), `"abc" is not 32 hex digits`},
		{"bad size", field("Checksums-Sha256", list(orig.SHA256+" -1 "+orig.Name)), `"-1" is not a size`},
		{"a directory", field("Checksums-Sha256", list(orig.SHA256+" 1 ../hello_1.tar.gz")), "is not the name of a file of a source package"},
		{"twice", field("Checksums-Sha256", list(sha(orig), sha(orig))), "lists " + orig.Name + " twice"},
		{"another source", field("Checksums-Sha256", list(orig.SHA256+" 1 other_1.tar.gz")), "which is not a file of the source hello"},
		{"itself", field("Checksums-Sha256", list(orig.SHA256+" 1 hello_1.0.0-1.dsc")), "the .dsc names itself"},
		{"lists disagree on a size", field("Files", list(fmt.Sprintf("%s %d %s", orig.MD5, orig.Size+1, orig.Name), md5(debian))),
			"Files and Checksums-Sha256 disagree on " + orig.Name},
		{"Files lists more", field("Files", list(md5(orig), md5(debian), orig.MD5+" 1 hello_x.tar.gz")), "do not list the same files"},
		{"Checksums-Sha1 lacks one", field("Checksums-Sha1", list(fmt.Sprintf("%s %d %s", orig.SHA1, orig.Size, orig.Name))),
			"Checksums-Sha1 and Checksums-Sha256 disagree on " + debian.Name},
		{"too many", field("Checksums-Sha256", list(tooMany...)), "names 65 files, at most 64"},
		{"two paragraphs", para + "\nOther: x\n", "a second paragraph"},
		{"no signature block", "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n" + para, "has no -----BEGIN PGP SIGNATURE-----"},
		{"no end", "-----BEGIN PGP SIGNED MESSAGE-----\n\n" + para + "-----BEGIN PGP SIGNATURE-----\nAAAA\n", "has no -----END PGP SIGNATURE-----"},
		{"text after the end", "-----BEGIN PGP SIGNED MESSAGE-----\n\n" + para + "-----BEGIN PGP SIGNATURE-----\n-----END PGP SIGNATURE-----\nX: y\n",
			"has text after"},
		{"a dash not escaped", "-----BEGIN PGP SIGNED MESSAGE-----\n\n-x\n" + para + "-----BEGIN PGP SIGNATURE-----\n-----END PGP SIGNATURE-----\n",
			"not dash-escaped"},
		{"too large", para + "X-Pad: " + strings.Repeat("a", MaxDsc) + "\n", "at most 1048576"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDsc([]byte(tc.dsc))
			if !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ParseDsc = %v, want ErrInvalid with %q", err, tc.want)
			}
		})
	}
}
