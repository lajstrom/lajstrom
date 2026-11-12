//go:build integration

package facade

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegrationSpoolBudget is #427 against the registry at LAJSTROM_TEST_REGISTRY, with
// registries[].uploads.spoolUnknownLength on and server.spoolBudget of 1 MiB: while a chunk of
// unknown length of one upload is spooled and holds 768 KiB, another upload's chunk of 512 KiB
// is 503 UNAVAILABLE with Retry-After: 5, and the registry's session of that upload keeps its
// offset; a chunk of 128 KiB passes meanwhile, and the refused chunk passes once the first is
// done. Both blobs close with their digest and hold their bytes; nothing is left in TMPDIR.
func TestIntegrationSpoolBudget(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	generic.SetSpoolBudget(1 << 20)
	t.Cleanup(func() { generic.SetSpoolBudget(0) })
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:facade-it-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	must(t, st.CreateRepo(ctx, &index.Repo{Format: "oci", Name: "images", Registry: "it", Prefix: "oci/images", Kind: "hosted"}))
	cfg := config.Registry{Name: "it", Type: generic.Type, URL: "http://" + addr, Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth:    config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"), Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")},
		Uploads: config.RegistryUploads{SpoolUnknownLength: true}}
	logs := &logBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	p, err := generic.New(cfg, log)
	must(t, err)
	master, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
	must(t, err)
	tokens, err := keys.NewTokens(keys.TokenOptions{Index: st, Master: master, Log: log})
	must(t, err)
	srv := httptest.NewServer(New(Options{Index: st, Providers: provider.Map{"it": p}, Tokens: tokens, ExternalURL: "https://art.test",
		Log: log, Ingest: ingest.New(ingest.Options{Providers: provider.Map{"it": p}, Index: st, Log: log})}))
	t.Cleanup(srv.Close)
	name := "oci/images/it-" + randomHex(t)

	// open starts an upload with a first chunk of known length, which is never spooled, and
	// returns its location.
	open := func(first []byte) string {
		t.Helper()
		resp, body := request(t, http.MethodPost, srv.URL+"/v2/"+name+"/blobs/uploads/", nil, 0)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST: %d %s", resp.StatusCode, body)
		}
		loc := srv.URL + resp.Header.Get("Location")
		if resp, body = request(t, http.MethodPatch, loc, bytes.NewReader(first), int64(len(first)), "Content-Type", octetStream,
			"Content-Range", fmt.Sprintf("0-%d", len(first)-1)); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("first PATCH: %d %s", resp.StatusCode, body)
		}
		return srv.URL + resp.Header.Get("Location")
	}
	// patch sends a chunk of unknown length, chunked encoding, and returns the answer.
	patch := func(loc string, chunk io.Reader) (*http.Response, []byte) {
		t.Helper()
		return request(t, http.MethodPatch, loc, chunk, -1, "Content-Type", octetStream)
	}
	part := func(c byte, n int) []byte { return bytes.Repeat([]byte{c}, n) }
	a, b := open(part('a', 1024)), open(part('b', 1024))

	pr, pw := io.Pipe()
	held := make(chan *http.Response, 1)
	go func() {
		resp, _ := patch(a, pr)
		held <- resp
	}()
	if _, err := pw.Write(part('c', 768<<10)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the spool has written what it read
	resp, body := patch(b, bytes.NewReader(part('d', 512<<10)))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "5" ||
		!strings.Contains(string(body), `"code":"UNAVAILABLE"`) {
		t.Errorf("a chunk past the budget: %d, Retry-After %q, %s; want 503 UNAVAILABLE with Retry-After: 5", resp.StatusCode,
			resp.Header.Get("Retry-After"), body)
	}
	if !strings.Contains(logs.String(), "server.spoolBudget") {
		t.Errorf("the refusal is not logged with server.spoolBudget:\n%s", logs)
	}
	if resp, body = request(t, http.MethodGet, b, nil, 0); resp.StatusCode != http.StatusNoContent || resp.Header.Get("Range") != "0-1023" {
		t.Errorf("the session of the refused chunk: %d, Range %q, %s; want its first 1024 bytes", resp.StatusCode, resp.Header.Get("Range"), body)
	}
	if resp, body = patch(b, bytes.NewReader(part('e', 128<<10))); resp.StatusCode != http.StatusAccepted {
		t.Errorf("a chunk that fits the budget beside the other: %d %s", resp.StatusCode, body)
	}
	_ = pw.Close()
	if resp := <-held; resp.StatusCode != http.StatusAccepted {
		t.Errorf("the chunk that held the budget: %d", resp.StatusCode)
	}
	if resp, body = patch(b, bytes.NewReader(part('d', 512<<10))); resp.StatusCode != http.StatusAccepted {
		t.Errorf("the refused chunk again, once the other is done: %d %s", resp.StatusCode, body)
	}
	for _, up := range []struct {
		loc  string
		blob []byte
	}{
		{a, append(part('a', 1024), part('c', 768<<10)...)},
		{b, append(append(part('b', 1024), part('e', 128<<10)...), part('d', 512<<10)...)},
	} {
		loc, _ := patchLocation(t, srv.URL, up.loc)
		d := digest.FromBytes(up.blob)
		if resp, body := request(t, http.MethodPut, loc+sep(loc)+"digest="+d.String(), nil, 0); resp.StatusCode != http.StatusCreated {
			t.Fatalf("PUT ?digest=%s: %d %s", d, resp.StatusCode, body)
		}
		rr, err := p.Repository(ctx, name)
		must(t, err)
		if ok, err := rr.Exists(ctx, ocispec.Descriptor{Digest: d, Size: int64(len(up.blob))}); err != nil || !ok {
			t.Errorf("the registry has %s: %v, %v", d, ok, err)
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("files left in the temporary directory: %v", left)
	}
}

// patchLocation returns the current location of the upload at loc, from a GET of its status.
func patchLocation(t *testing.T, base, loc string) (string, *http.Response) {
	t.Helper()
	resp, body := request(t, http.MethodGet, loc, nil, 0)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("GET %s: %d %s", loc, resp.StatusCode, body)
	}
	return base + resp.Header.Get("Location"), resp
}

// sep is the separator before a query parameter added to u.
func sep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}
