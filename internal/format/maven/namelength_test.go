package maven

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestNameLength: below the instance prefix the registry takes 255 bytes less the prefix's
// length (the generic provider's MaxNameLength): a version whose OCI repository name is exactly
// that long deploys, and the first file of one byte more is 400 naming the limit, and nothing of
// it is pushed; without a prefix the limit is 255, as before (#661).
func TestNameLength(t *testing.T) {
	for _, prefix := range []string{"", "lajstrom/"} {
		t.Run("prefix "+prefix, func(t *testing.T) {
			e := newDeployEnv(t, nil)
			limit := 255 - len(prefix)
			e.fake.MaxNameLength = limit
			group := strings.Repeat("g", limit-len("maven/releases//lib-core"))
			jar, ji := testutil.Jar(testutil.JarOptions{GroupID: group})
			pom, pi := testutil.POM(testutil.POMOptions{GroupID: group})
			e.wantPut(ji.Path, jar, http.StatusCreated)
			e.wantPut(pi.Path, pom, http.StatusCreated)
			e.wantPut(group+"/lib-core/maven-metadata.xml", []byte(`<metadata><version>1.2.0</version></metadata>`), http.StatusCreated)
			pushes := len(e.fake.Pushes())

			longer, li := testutil.Jar(testutil.JarOptions{GroupID: group + "g"})
			rr := e.put("alice", li.Path, longer)
			want := fmt.Sprintf("the OCI repository maven/releases/%sg/lib-core is %d bytes long, and the registry takes at most %d", group, limit+1, limit)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), want) {
				t.Errorf("PUT of %d bytes of name = %d %s, want 400 with %q", limit+1, rr.Code, rr.Body, want)
			}
			if n := len(e.fake.Pushes()) - pushes; n != 0 || e.fake.OpenUploads() != 0 {
				t.Errorf("%d blobs pushed and %d sessions open for a name the registry does not take", n, e.fake.OpenUploads())
			}
		})
	}
}
