package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// capsOf returns a Capabilities lookup that answers c for every registry and counts its calls.
func capsOf(c provider.Capabilities, err error, calls *int) func(context.Context, string) (provider.Capabilities, error) {
	return func(context.Context, string) (provider.Capabilities, error) {
		*calls++
		return c, err
	}
}

// TestCreateRegistryNames is #663: a repo on a registry that takes none of its names, by the
// depth or the length its capabilities know, is refused with the field registry and the limit,
// and nothing is created; a registry that takes them, or whose capabilities are not known,
// gets the repo.
func TestCreateRegistryNames(t *testing.T) {
	for _, tt := range []struct {
		name    string
		caps    provider.Capabilities
		err     error
		wantErr []string // substrings; nil means created
	}{
		{name: "docker hub", caps: provider.Capabilities{MaxNameDepth: 1},
			wantErr: []string{`registry: "local" takes names of depth 1 at most`, "capabilities.maxNameDepth", "raw/scratch/<package>"}},
		{name: "two components", caps: provider.Capabilities{MaxNameDepth: 2}, wantErr: []string{"depth 2 at most"}},
		{name: "three components", caps: provider.Capabilities{MaxNameDepth: 3}},
		{name: "gitlab", caps: provider.Capabilities{MaxNameDepth: 4}},
		{name: "no limit", caps: provider.Capabilities{}},
		{name: "too long", caps: provider.Capabilities{MaxNameLength: 12},
			wantErr: []string{`registry: "local" takes at most 12 bytes`, "raw/scratch/<package>, has 13"}},
		{name: "long enough", caps: provider.Capabilities{MaxNameLength: 13}},
		{name: "not known", err: errors.New("ping local: connection refused")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Options{})
			calls := 0
			f.repos.Capabilities = capsOf(tt.caps, tt.err, &calls)
			_, err := f.repos.Create(t.Context(), CreateRequest{Format: "raw", Name: "scratch", Registry: "local"}, Origin{})
			if calls != 1 {
				t.Errorf("Capabilities asked %d times, want once", calls)
			}
			_, getErr := f.repos.Get(t.Context(), "raw", "scratch")
			if tt.wantErr == nil {
				if err != nil || getErr != nil {
					t.Fatalf("Create = %v, Get = %v; want the repo created", err, getErr)
				}
				return
			}
			if !errors.Is(err, format.ErrInvalid) {
				t.Fatalf("Create = %v, want format.ErrInvalid", err)
			}
			for _, s := range tt.wantErr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("Create = %q, want it to contain %q", err, s)
				}
			}
			if getErr == nil {
				t.Error("the refused repo was created")
			}
			if n := len(auditOf(t, f.st, "repo.create")); n != 0 {
				t.Errorf("%d repo.create events, want none", n)
			}
		})
	}
	t.Run("not known is logged", func(t *testing.T) {
		f := newFixture(t, Options{})
		calls := 0
		f.repos.Capabilities = capsOf(provider.Capabilities{}, errors.New("ping local: connection refused"), &calls)
		if _, err := f.repos.Create(t.Context(), CreateRequest{Format: "oci", Name: "images", Registry: "local"}, Origin{}); err != nil {
			t.Fatal(err)
		}
		if logs := f.logs.String(); !strings.Contains(logs, `"level":"WARN"`) || !strings.Contains(logs, `"registry":"local"`) ||
			!strings.Contains(logs, "connection refused") {
			t.Errorf("logs %s, want a warning naming the registry and the cause", logs)
		}
	})
}

// The API answers a repo on a registry that takes none of its names 400 INVALID; a group, which
// has no registry, is not checked.
func TestCreateRegistryNamesHTTP(t *testing.T) {
	f := newGroupFixture(t)
	calls := 0
	f.repos.Capabilities = capsOf(provider.Capabilities{MaxNameDepth: 1}, nil, &calls)
	rr := f.do(http.MethodPost, "/api/v1/repos", `{"format": "npm", "name": "hub", "registry": "local"}`, admin)
	wantError(t, rr, http.StatusBadRequest, "INVALID", `registry: "local" takes names of depth 1 at most`, "npm/hub/<package>")
	body := `{"format": "npm", "name": "all", "kind": "group", "group": {"members": ["internal", "npmjs"]}}`
	wantStatus(t, f.do(http.MethodPost, "/api/v1/repos", body, admin), http.StatusCreated)
	if calls != 1 {
		t.Errorf("Capabilities asked %d times, want once: not for the group", calls)
	}
}
