package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Control plane: config_state, the one row of the configuration last recorded (#1284).

const configStateCols = "fingerprint, config, release, host, recorded_at"

func configStateDests(s *index.ConfigState) []any {
	return []any{&s.Fingerprint, jsonDest[map[string]json.RawMessage]{&s.Config}, &s.Release, &s.Host, timeDest{&s.RecordedAt}}
}

// GetConfigState returns the row of config_state.
func (c *conn) GetConfigState(ctx context.Context) (*index.ConfigState, error) {
	return getOne(ctx, c, "config_state", configStateDests, "SELECT "+configStateCols+" FROM config_state WHERE id = 1")
}

// PutConfigState inserts the row, or replaces the row whose fingerprint is old, in one statement:
// a row that is not the one expected changes nothing and is the ErrConflict.
func (c *conn) PutConfigState(ctx context.Context, s *index.ConfigState, old string) error {
	stamp(&s.RecordedAt, now())
	cfg := s.Config
	if cfg == nil {
		cfg = map[string]json.RawMessage{}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config_state: %w", err)
	}
	args := []any{s.Fingerprint, string(raw), s.Release, s.Host, c.d.EncodeTime(s.RecordedAt)}
	var n int
	if old == "" {
		n, err = c.exec(ctx, "config_state",
			"INSERT INTO config_state (id, "+configStateCols+") VALUES (1, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING", args...)
	} else {
		n, err = c.exec(ctx, "config_state",
			"UPDATE config_state SET fingerprint = ?, config = ?, release = ?, host = ?, recorded_at = ? WHERE id = 1 AND fingerprint = ?",
			append(args, old)...)
	}
	if err == nil && n == 0 {
		err = fmt.Errorf("%w: config_state: another configuration was recorded in between", index.ErrConflict)
	}
	return err
}
