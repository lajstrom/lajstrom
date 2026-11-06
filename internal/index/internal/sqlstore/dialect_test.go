package sqlstore

import (
	"testing"
	"time"
)

func TestRebind(t *testing.T) {
	tests := []struct {
		query, numbered string
	}{
		{"SELECT 1", "SELECT 1"},
		{"SELECT a FROM t WHERE b = ?", "SELECT a FROM t WHERE b = $1"},
		{"INSERT INTO t (a, b, c) VALUES (?, ?, ?)", "INSERT INTO t (a, b, c) VALUES ($1, $2, $3)"},
		{"x LIMIT ? OFFSET ?", "x LIMIT $1 OFFSET $2"},
	}
	for _, tt := range tests {
		for _, d := range []Dialect{{}, {Numbered: true}} {
			want := tt.query
			if d.Numbered {
				want = tt.numbered
			}
			if got := d.Rebind(tt.query); got != want {
				t.Errorf("Rebind(%q), numbered %v = %q, want %q", tt.query, d.Numbered, got, want)
			}
		}
	}
}

// TestTimeText checks the SQLite encoding: fixed width, so that text order is time order, and
// decoded back exactly.
func TestTimeText(t *testing.T) {
	d := Dialect{TimeText: true}
	zone := time.FixedZone("UTC+3", 3*3600)
	times := []time.Time{
		{},
		time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 6, 13, 0, 0, 500, zone), // 10:00:00.0000005Z, truncated to 10:00:00Z
		time.Date(2026, 11, 6, 10, 0, 0, 1000, time.UTC),
		time.Date(2026, 11, 6, 10, 0, 0, 999999000, time.UTC),
		time.Date(2026, 11, 6, 10, 0, 1, 0, time.UTC),
	}
	prev := ""
	for _, tm := range times {
		text, ok := d.EncodeTime(tm).(string)
		if !ok || len(text) != len(timeLayout) {
			t.Fatalf("EncodeTime(%v) = %v, want text of %d bytes", tm, d.EncodeTime(tm), len(timeLayout))
		}
		if text < prev {
			t.Errorf("EncodeTime(%v) = %q sorts before %q", tm, text, prev)
		}
		prev = text
		back, err := decodeTime(text)
		if err != nil || !back.Equal(Normalize(tm)) || back.Location() != time.UTC {
			t.Errorf("decodeTime(%q) = %v, %v; want %v", text, back, err, Normalize(tm))
		}
	}
	if pg := (&Dialect{}).EncodeTime(times[2]); pg != Normalize(times[2]) {
		t.Errorf("EncodeTime on Postgres = %v, want %v", pg, Normalize(times[2]))
	}
}
