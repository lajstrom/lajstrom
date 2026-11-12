package deb

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestNameLength: below the instance prefix the registry takes 255 bytes less the prefix's
// length (the generic provider's MaxNameLength): a package whose OCI repository name is exactly
// that long is stored, one byte more is 400 naming the limit, and nothing of it is pushed;
// without a prefix the limit is 255, as before (#661).
func TestNameLength(t *testing.T) {
	files := map[string]string{"usr/x": "x"} // the default paths hold the name, too long for a tar header
	for _, prefix := range []string{"", "lajstrom/"} {
		t.Run("prefix "+prefix, func(t *testing.T) {
			e := newAptEnv(t, envOptions{})
			limit := 255 - len(prefix)
			e.prov.MaxNameLength = limit
			name := strings.Repeat("a", limit-len("apt/ubuntu/"))
			fits, _ := testutil.Deb(testutil.DebOptions{Name: name, Files: files})
			uploaded(t, e.put("suite=jammy&component=main", fits))
			pushes := len(e.prov.Pushes())

			longer, _ := testutil.Deb(testutil.DebOptions{Name: name + "b", Files: files})
			rr := e.put("suite=jammy&component=main", longer)
			want := fmt.Sprintf("the OCI repository apt/ubuntu/%sb is %d bytes long, and the registry takes at most %d", name, limit+1, limit)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), want) {
				t.Errorf("PUT of %d bytes of name = %d %s, want 400 with %q", limit+1, rr.Code, rr.Body, want)
			}
			if n := len(e.prov.Pushes()) - pushes; n != 0 || e.prov.OpenUploads() != 0 {
				t.Errorf("%d blobs pushed and %d sessions open for a name the registry does not take", n, e.prov.OpenUploads())
			}
		})
	}
}
