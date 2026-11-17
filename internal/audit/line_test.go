package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// row is an audit row with every field set.
var row = index.AuditEvent{ID: 42, At: time.Date(2026, 11, 17, 9, 30, 1, 123456000, time.FixedZone("CET", 3600)),
	Principal: "user:01a1c136-5180-7b5e-95ce-428e99e68887", PrincipalName: "ci-bot", Action: "push", Repo: "raw/scratch",
	Subject: "pkg 1.0", Digest: digest.FromString("m"), IP: "10.0.0.1", Details: json.RawMessage(`{"requestId": "r-1", "via": "raw/all"}`),
	State: index.AuditFailed, Error: "the registry said no"}

// TestLineIsTheEntry: in the json format the line of a row is its Entry as the API answers it,
// besides time, level and msg, details an object; empty fields are left out as the API leaves
// them out.
func TestLineIsTheEntry(t *testing.T) {
	bare := index.AuditEvent{ID: 7, At: row.At, Principal: "anonymous", Action: "config.change", State: index.AuditOK}
	for _, tt := range []struct {
		name string
		ev   index.AuditEvent
		kind string
	}{
		{"every field", row, "service"},
		{"the empty fields left out", bare, ""},
	} {
		var buf bytes.Buffer
		Lines(slog.New(slog.NewJSONHandler(&buf, nil)), func(context.Context, string) (string, error) { return tt.kind, nil })(
			[]index.AuditEvent{tt.ev})
		var line map[string]any
		if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
			t.Fatalf("%s: %q: %v", tt.name, buf.String(), err)
		}
		if line["msg"] != "audit" || line["level"] != "INFO" {
			t.Errorf("%s: msg %v, level %v; want audit at INFO", tt.name, line["msg"], line["level"])
		}
		delete(line, "time")
		delete(line, "level")
		delete(line, "msg")
		api, err := json.Marshal(NewEntry(tt.ev, tt.kind))
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]any
		if err := json.Unmarshal(api, &want); err != nil {
			t.Fatal(err)
		}
		if d := cmp.Diff(want, line); d != "" {
			t.Errorf("%s: the line is not the API's entry (-api +line):\n%s", tt.name, d)
		}
	}
}

// TestLineText: in the text format the line has the same fields, details as its JSON text.
func TestLineText(t *testing.T) {
	var buf bytes.Buffer
	Lines(slog.New(slog.NewTextHandler(&buf, nil)), func(context.Context, string) (string, error) { return "", nil })(
		[]index.AuditEvent{row})
	got := buf.String()
	for _, want := range []string{"level=INFO msg=audit id=42 at=2026-11-17T08:30:01.123456Z principal=user:01a1c136-5180-7b5e-95ce-428e99e68887",
		" action=push ", " state=failed ", ` error="the registry said no" `, `details="{\"requestId\": \"r-1\", \"via\": \"raw/all\"}"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the line %q lacks %q", got, want)
		}
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("%d lines for a row: %q", strings.Count(got, "\n"), got)
	}
}

// TestLinesKinds: a line per row in the order given; the kind of a principal is read once, and a
// kind that cannot be read leaves principalKind out of the line, which is written all the same.
func TestLinesKinds(t *testing.T) {
	reads := map[string]int{}
	kind := func(_ context.Context, p string) (string, error) {
		reads[p]++
		switch p {
		case "user:svc":
			return "service", nil
		case "user:gone":
			return "", errors.New("the database is gone")
		}
		return "", nil
	}
	var buf bytes.Buffer
	observe := Lines(slog.New(slog.NewJSONHandler(&buf, nil)), kind)
	var rows []index.AuditEvent
	for i, p := range []string{"user:svc", "anonymous", "user:gone", "user:svc", "user:gone"} {
		rows = append(rows, index.AuditEvent{ID: int64(i + 1), Principal: p, Action: "push", State: index.AuditOK})
	}
	observe(rows[:2])
	observe(rows[2:])
	var kinds []string
	for l := range strings.Lines(buf.String()) {
		var line struct{ Principal, PrincipalKind string }
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, line.Principal+"="+line.PrincipalKind)
	}
	if d := cmp.Diff([]string{"user:svc=service", "anonymous=", "user:gone=", "user:svc=service", "user:gone="}, kinds); d != "" {
		t.Errorf("the lines' kinds (-want +got):\n%s", d)
	}
	if reads["user:svc"] != 1 || reads["user:gone"] != 2 {
		t.Errorf("reads %v: want the kind of user:svc read once, and one that failed read again", reads)
	}
}
