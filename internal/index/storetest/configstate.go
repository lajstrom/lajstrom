package storetest

import (
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testConfigState: the one row of config_state (#1284). It is put when there is none or over the
// fingerprint it replaces, never over another; its values come back as the same JSON, whatever
// spelling the database keeps; a put in a transaction that rolls back is no put.
func testConfigState(t *testing.T, s index.Store) {
	ctx := t.Context()
	wantErr(t, "GetConfigState of none", second(s.GetConfigState(ctx)), index.ErrNotFound)

	first := index.ConfigState{Fingerprint: "sha256:a", Release: "v1.0.0", Host: "r1", RecordedAt: at(0),
		Config: map[string]json.RawMessage{
			"auth.anonymousRead":    json.RawMessage(`false`),
			"server.trustedProxies": json.RawMessage(`["10.0.0.0/8"]`),
			"server.tls.certFile":   json.RawMessage(`{"path":"/etc/tls/cert.pem","sha256":"41d0"}`),
		}}
	must(t, s.PutConfigState(ctx, &first, ""))
	got, err := s.GetConfigState(ctx)
	must(t, err)
	sameConfig(t, "GetConfigState", first, *got)
	for _, old := range []string{"", "sha256:b"} {
		other := index.ConfigState{Fingerprint: "sha256:c", Release: "v1.0.0", Host: "r2"}
		wantErr(t, "PutConfigState over "+strconv.Quote(old), s.PutConfigState(ctx, &other, old), index.ErrConflict)
	}

	errBoom := errors.New("boom")
	err = s.Tx(ctx, func(w index.Writer) error {
		must(t, w.PutConfigState(ctx, &index.ConfigState{Fingerprint: "sha256:d", Release: "v1.1.0", Host: "r1"}, "sha256:a"))
		return errBoom
	})
	wantErr(t, "Tx", err, errBoom)
	got, err = s.GetConfigState(ctx)
	must(t, err)
	sameConfig(t, "after the rollback", first, *got)

	next := index.ConfigState{Fingerprint: "sha256:e", Release: "v1.1.0", Host: "r2", RecordedAt: at(60),
		Config: map[string]json.RawMessage{"auth.anonymousRead": json.RawMessage(`true`)}}
	must(t, s.PutConfigState(ctx, &next, "sha256:a"))
	got, err = s.GetConfigState(ctx)
	must(t, err)
	sameConfig(t, "the configuration replaced", next, *got)
}

// testConfigStateRace: replicas that start together with one configuration and record it at once
// leave one row, theirs whose put succeeded; the others get ErrConflict.
func testConfigStateRace(t *testing.T, s index.Store) {
	ctx := t.Context()
	const replicas = 8
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Go(func() {
			c := index.ConfigState{Fingerprint: "sha256:f", Release: "v1.0.0", Host: "r" + strconv.Itoa(i)}
			errs[i] = s.PutConfigState(ctx, &c, "")
		})
	}
	wg.Wait()
	stored, err := s.GetConfigState(ctx)
	must(t, err)
	winners := 0
	for i, err := range errs {
		if err != nil {
			wantErr(t, "PutConfigState of replica "+strconv.Itoa(i), err, index.ErrConflict)
			continue
		}
		winners++
		if stored.Host != "r"+strconv.Itoa(i) {
			t.Errorf("replica %d recorded the configuration, the stored one is %s's", i, stored.Host)
		}
	}
	if winners != 1 {
		t.Fatalf("%d replicas recorded the configuration, want 1", winners)
	}
}

// sameConfig compares two ConfigStates, their values as decoded JSON: jsonb keeps another
// spelling of the same value.
func sameConfig(t *testing.T, what string, want, got index.ConfigState) {
	t.Helper()
	decode := func(c index.ConfigState) map[string]any {
		m := map[string]any{}
		for k, v := range c.Config {
			var x any
			must(t, json.Unmarshal(v, &x))
			m[k] = x
		}
		return m
	}
	same(t, what+": config", decode(want), decode(got))
	want.Config, got.Config = nil, nil
	same(t, what, want, got)
}
