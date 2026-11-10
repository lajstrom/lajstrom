package npm

import (
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
)

func TestCheckName(t *testing.T) {
	for _, name := range []string{"lodash", "@acme/widgets", "@types/node", "a", "left-pad", "JSONStream", "x.y_z-1", "@a/b.c", "~(x)*!'", strings.Repeat("a", 214)} {
		if err := CheckName(name); err != nil {
			t.Errorf("CheckName(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", ".hidden", "_private", "node_modules", "Favicon.ico", "a b", "a/b", "@acme", "@acme/", "@/x",
		"@a/b/c", "@a/..", "@./x", "ü", "a%2fb", "a:b", strings.Repeat("a", 215), "@acme/wid gets"} {
		if err := CheckName(name); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("CheckName(%q) = %v, want ErrInvalid", name, err)
		}
	}
	for name, ok := range map[string]bool{"widgets": true, "@acme/widgets": true, "JSONStream": false, "@Acme/widgets": false} {
		if err := CheckNewName(name); (err == nil) != ok {
			t.Errorf("CheckNewName(%q) = %v", name, err)
		}
	}
}

func TestPackagePathAndFilename(t *testing.T) {
	for _, tc := range []struct{ name, path, file string }{
		{"lodash", "lodash", "lodash-1.2.0.tgz"},
		{"@acme/widgets", "acme/widgets", "widgets-1.2.0.tgz"},
		{"@types/node", "types/node", "node-1.2.0.tgz"},
		{"JSONStream", "jsonstream", "JSONStream-1.2.0.tgz"},
	} {
		if p, f := packagePath(tc.name), Filename(tc.name, "1.2.0"); p != tc.path || f != tc.file {
			t.Errorf("%s: path %q, file %q; want %q, %q", tc.name, p, f, tc.path, tc.file)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for _, v := range []string{"0.0.0", "1.2.3", "10.20.30", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-0.3.7", "1.0.0-x.7.z.92",
		"1.0.0-rc-1", "1.0.0--", "9007199254740991.0.0"} {
		if err := CheckVersion(v); err != nil {
			t.Errorf("CheckVersion(%q) = %v", v, err)
		}
	}
	for _, v := range []string{"", "1", "1.2", "1.2.3.4", "v1.2.3", "01.2.3", "1.02.3", "1.2.3-", "1.2.3-01", "1.2.3-a..b",
		"1.2.3+build", "1.2.3-rc.1+build.5", "1.2.3-ü", " 1.2.3", "9007199254740992.0.0", "1.2.3-" + strings.Repeat("a", 256)} {
		if err := CheckVersion(v); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("CheckVersion(%q) = %v, want ErrInvalid", v, err)
		}
	}
}
