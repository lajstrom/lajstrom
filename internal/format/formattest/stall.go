package formattest

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
)

// ErrStalled is the error of a read of a request body that server.stallTimeout cut, as
// net/http returns it to the handler: the read deadline of the client's connection passed.
var ErrStalled error = &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}

// Stalled returns a request body that gives the bytes of r and then fails with ErrStalled: a
// client that stopped sending part way (#576).
func Stalled(r io.Reader) io.Reader {
	return io.MultiReader(r, stallReader{})
}

type stallReader struct{}

func (stallReader) Read([]byte) (int, error) { return 0, ErrStalled }

// CheckStalled fails t when rr is not a frontend's answer to a Stalled body: 408 with the
// message of format.ClientMessage, which does not name the connection.
func CheckStalled(t testing.TB, rr *httptest.ResponseRecorder) {
	t.Helper()
	body := rr.Body.String()
	if want := format.ClientMessage(ErrStalled); rr.Code != http.StatusRequestTimeout || !strings.Contains(body, want) ||
		strings.Contains(body, ErrStalled.Error()) {
		t.Errorf("stalled body: %d %q, want 408 with %q", rr.Code, body, want)
	}
}
