package postgres

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestConnectTimeout checks the default bound of a connection attempt (#1123): 5 s when the DSN
// sets no connect_timeout or sets 0, the DSN's own value otherwise.
func TestConnectTimeout(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "")
	for _, tt := range []struct {
		dsn  string
		want time.Duration
	}{
		{"postgres://lajstrom@db:5432/lajstrom", defaultConnectTimeout},
		{"postgres://lajstrom@db:5432/lajstrom?connect_timeout=0", defaultConnectTimeout},
		{"postgres://lajstrom@db:5432/lajstrom?connect_timeout=2", 2 * time.Second},
		{"host=db user=lajstrom connect_timeout=30", 30 * time.Second},
	} {
		cfg, err := pgx.ParseConfig(tt.dsn)
		if err != nil {
			t.Fatal(err)
		}
		connectTimeout(cfg)
		if cfg.ConnectTimeout != tt.want {
			t.Errorf("%s: ConnectTimeout = %v, want %v", tt.dsn, cfg.ConnectTimeout, tt.want)
		}
	}
}
