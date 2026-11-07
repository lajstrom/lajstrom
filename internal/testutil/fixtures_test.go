package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFixtures writes the fixtures that were checked with the real tools (the comments at
// the top of deb_test.go, wheel_test.go and jar_test.go) to the directory named by
// LAJSTROM_FIXTURES_OUT, an existing directory, and is skipped without it:
//
//	deb/hello_1.0.0-1_amd64.deb, deb/hello_1.0.0-1_all.deb
//	pypi/my_pkg-1.0.0-py3-none-any.whl, pypi/my_pkg-1.0.0.tar.gz
//	maven/com/example/lib-api/1.0.0/lib-api-1.0.0.{jar,pom}
//	maven/com/example/lib-core/1.2.0/lib-core-1.2.0.{jar,pom}, the POM depends on lib-api
//
// Every maven file has its .md5, .sha1, .sha256 and .sha512 next to it.
func TestWriteFixtures(t *testing.T) {
	out := os.Getenv("LAJSTROM_FIXTURES_OUT")
	if out == "" {
		t.Skip("LAJSTROM_FIXTURES_OUT is not set")
	}
	root, err := os.OpenRoot(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	write := func(name string, b []byte) {
		t.Helper()
		p := filepath.FromSlash(name)
		if err := root.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, arch := range []string{"amd64", "all"} {
		b, want := Deb(DebOptions{Arch: arch})
		write("deb/"+want.Filename, b)
	}
	whl, wheel := Wheel(WheelOptions{})
	write("pypi/"+wheel.Filename, whl)
	sdist, sd := Sdist(SdistOptions{})
	write("pypi/"+sd.Filename, sdist)

	maven := func(path string, b []byte, sums Checksums) {
		write("maven/"+path, b)
		for ext, sum := range map[string]string{".md5": sums.MD5, ".sha1": sums.SHA1, ".sha256": sums.SHA256, ".sha512": sums.SHA512} {
			write("maven/"+path+ext, []byte(sum))
		}
	}
	api := JarOptions{ArtifactID: "lib-api", Version: "1.0.0"}
	jar, j := Jar(api)
	maven(j.Path, jar, j.Checksums)
	pom, p := POM(POMOptions{ArtifactID: api.ArtifactID, Version: api.Version})
	maven(p.Path, pom, p.Checksums)
	jar, j = Jar(JarOptions{})
	maven(j.Path, jar, j.Checksums)
	pom, p = POM(POMOptions{Dependencies: []DependencyOptions{{GroupID: "com.example", ArtifactID: api.ArtifactID, Version: api.Version}}})
	maven(p.Path, pom, p.Checksums)
}
