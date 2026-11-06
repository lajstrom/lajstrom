package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// stubPlugin is a format.Plugin with a name and nothing else.
type stubPlugin string

func (p stubPlugin) Name() string                    { return string(p) }
func (p stubPlugin) ArtifactType() string            { return "application/vnd.lajstrom." + string(p) + ".v1" }
func (stubPlugin) EscapeName(string) (string, error) { return "", format.ErrUnsupported }
func (stubPlugin) Routes(format.Router, format.Deps) {}
func (stubPlugin) Build(context.Context, index.Repo, format.Upload) (*format.Built, error) {
	return nil, format.ErrUnsupported
}

func (stubPlugin) Validate(context.Context, index.Repo, ocispec.Manifest, format.BlobFetcher) (*format.Entry, error) {
	return nil, format.ErrUnsupported
}

func (stubPlugin) RenderIndex(context.Context, index.Repo, format.IndexScope, index.Reader) ([]format.Document, error) {
	return nil, format.ErrUnsupported
}

// newTestServer returns a Server that logs JSON into the returned buffer.
func newTestServer(t *testing.T, o Options) (*Server, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	if o.Listen == "" {
		o.Listen = "127.0.0.1:0"
	}
	o.Log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, &buf
}

// logLines decodes the JSON log lines in buf.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line is not JSON: %v: %s", err, sc.Text())
		}
		lines = append(lines, m)
	}
	return lines
}

// findLog returns the first log line with message msg.
func findLog(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	for _, l := range logLines(t, buf) {
		if l["msg"] == msg {
			return l
		}
	}
	t.Fatalf("no log line %q in:\n%s", msg, buf)
	return nil
}
