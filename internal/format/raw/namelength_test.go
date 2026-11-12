package raw

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// longPath is a raw path of n bytes, components of at most 61 bytes, so that none is hashed.
func longPath(n int) string {
	p := strings.Repeat("a", (n-1)%61+1)
	for len(p) < n {
		p = strings.Repeat("a", 60) + "/" + p
	}
	return p
}

// TestNameLength: below the instance prefix the registry takes 255 bytes less the prefix's
// length (the generic provider's MaxNameLength): a path whose OCI repository name is exactly
// that long is stored, one byte more is 400 naming the limit, and nothing of it is pushed;
// without a prefix the limit is 255, as before (#661).
func TestNameLength(t *testing.T) {
	for _, prefix := range []string{"", "lajstrom/"} {
		t.Run("prefix "+prefix, func(t *testing.T) {
			e := newEnv(t, envOptions{})
			limit := 255 - len(prefix)
			e.prov.MaxNameLength = limit
			fits := longPath(limit - len("raw/scratch/"))
			e.put(fits, "fits")
			pushes := len(e.prov.Pushes())

			rr := e.do(http.MethodPut, target(fits+"b"), strings.NewReader("one byte more"))
			want := fmt.Sprintf("the OCI repository raw/scratch/%sb is %d bytes long, and the registry takes at most %d", fits, limit+1, limit)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), want) {
				t.Errorf("PUT of %d bytes of name = %d %s, want 400 with %q", limit+1, rr.Code, rr.Body, want)
			}
			if strings.Contains(rr.Body.String(), "lajstrom/") {
				t.Errorf("the answer names the prefix: %s", rr.Body)
			}
			if n := len(e.prov.Pushes()) - pushes; n != 0 || e.prov.OpenUploads() != 0 {
				t.Errorf("%d blobs pushed and %d sessions open for a name the registry does not take", n, e.prov.OpenUploads())
			}
		})
	}
}
