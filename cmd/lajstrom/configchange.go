package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// actionConfigChange is the audit action of a start whose configuration is not the one last
// recorded on the database (docs/spec/rbac.md §5, #1284).
const actionConfigChange = "config.change"

// maxConfigDetails bounds the details of a config.change row: the changes beyond are counted in
// truncated, and config_state holds them all.
const maxConfigDetails = 64 << 10

// configRecordAttempts bounds the compare-and-sets of recordConfig: each that fails is another
// replica that recorded another configuration in between.
const configRecordAttempts = 5

// configRecorder records the configuration of a start (recordConfig).
type configRecorder struct {
	st      index.Store
	host    string
	release string
	now     func() time.Time
	log     *slog.Logger
}

// recordConfig writes config.change, by anonymous with the source config, when the configuration
// c that this start runs with is not the one last recorded on the database (config_state), in the
// transaction that replaces the record by its fingerprint: of replicas that start together with
// one new configuration one writes, and the others find theirs recorded. One that loses to
// another configuration compares again with it. An error stops the start: no configuration takes
// effect without its row (docs/spec/rbac.md §5).
func (r configRecorder) recordConfig(ctx context.Context, c *config.Config) error {
	view, fp, err := c.Recorded(nil)
	if err != nil {
		return fmt.Errorf("record the configuration: %w", err)
	}
	for range configRecordAttempts {
		cur, err := r.st.GetConfigState(ctx)
		switch {
		case errors.Is(err, index.ErrNotFound):
			cur = nil
		case err != nil:
			return fmt.Errorf("record the configuration: %w", err)
		case cur.Fingerprint == fp:
			r.log.DebugContext(ctx, "the configuration is the one recorded", "fingerprint", fp)
			return nil
		}
		err = r.write(ctx, cur, view, fp)
		if !errors.Is(err, index.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("record the configuration: %d other configurations recorded meanwhile; start again", configRecordAttempts)
}

// write replaces the record cur (nil: none) by view, of fingerprint fp, and appends the row of the
// change, in one transaction; ErrConflict when another replica replaced cur meanwhile.
func (r configRecorder) write(ctx context.Context, cur *index.ConfigState, view map[string]json.RawMessage, fp string) error {
	old, before := "", map[string]json.RawMessage(nil)
	details := map[string]any{"release": r.release}
	if cur != nil {
		old, before = cur.Fingerprint, cur.Config
		details["previous"] = old
	} else {
		details["initial"] = true
	}
	api.Origin{Source: api.SourceConfig, Host: r.host}.AddTo(details)
	changes := config.DiffRecorded(before, view)
	raw, listed, err := configDetails(details, changes)
	if err != nil {
		return fmt.Errorf("record the configuration: %w", err)
	}
	at := r.now()
	err = r.st.Tx(ctx, func(w index.Writer) error {
		if err := w.PutConfigState(ctx, &index.ConfigState{Fingerprint: fp, Config: view, Release: r.release, Host: r.host,
			RecordedAt: at}, old); err != nil {
			return err
		}
		return w.AppendAudit(ctx, &index.AuditEvent{At: at, Principal: auth.Anonymous.String(), Action: actionConfigChange,
			Subject: fp, Details: raw, State: index.AuditOK})
	})
	if errors.Is(err, index.ErrConflict) {
		return err
	}
	if err != nil {
		return fmt.Errorf("record the configuration: %w", err)
	}
	keys := make([]string, 0, listed)
	for _, ch := range changes[:listed] {
		keys = append(keys, ch.Key)
	}
	r.log.InfoContext(ctx, "the configuration changed: recorded as config.change", "fingerprint", fp, "previous", old,
		"changes", len(changes), "keys", keys)
	return nil
}

// configDetails is details with the changes, as many as keep it within maxConfigDetails, in key
// order, and truncated the number of those left out; and how many it lists.
func configDetails(details map[string]any, changes []config.Change) (json.RawMessage, int, error) {
	base, err := json.Marshal(details)
	if err != nil {
		return nil, 0, err
	}
	const room = len(`,"changes":[],"truncated":1000000`)
	size, listed := len(base)+room, 0
	for _, ch := range changes {
		b, err := json.Marshal(ch)
		if err != nil {
			return nil, 0, err
		}
		if size+len(b)+1 > maxConfigDetails {
			break
		}
		size += len(b) + 1
		listed++
	}
	details["changes"] = append([]config.Change{}, changes[:listed]...)
	if n := len(changes) - listed; n > 0 {
		details["truncated"] = n
	}
	raw, err := json.Marshal(details)
	return raw, listed, err
}
