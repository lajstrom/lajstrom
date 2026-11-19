package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth/oidc"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// TestCheckRefreshKey: auth.oidc.refresh.interval takes a master key; without one serve refuses
// the configuration, exit status 2. Off, or without auth.oidc, it takes none.
func TestCheckRefreshKey(t *testing.T) {
	m, err := keys.ParseMaster(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	on := config.OIDC{Issuer: "https://idp.example", Refresh: config.OIDCRefresh{Interval: 15 * time.Minute}}
	for _, tt := range []struct {
		name string
		o    config.OIDC
		m    *keys.Master
		fail bool
	}{
		{"on with a master key", on, m, false},
		{"on without one", on, nil, true},
		{"off", config.OIDC{Issuer: "https://idp.example"}, nil, false},
		{"without auth.oidc", config.OIDC{Refresh: on.Refresh}, nil, false},
	} {
		err := checkRefreshKey(tt.o, tt.m)
		switch {
		case !tt.fail && err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case tt.fail && (!errors.Is(err, errRefreshWithoutKey) || exitStatus(err) != 2):
			t.Errorf("%s: %v (exit status %d), want the refusal with exit status 2", tt.name, err, exitStatus(err))
		}
	}
}

// TestWarnRefreshGrant: serve warns at start when the IdP's discovery document lists
// grant_types_supported without refresh_token, and says nothing when it lists it or none.
func TestWarnRefreshGrant(t *testing.T) {
	for _, tt := range []struct {
		name   string
		grants []string
		warn   bool
	}{
		{"without refresh_token", []string{"authorization_code", "implicit"}, true},
		{"with refresh_token", []string{"authorization_code", "refresh_token"}, false},
		{"none listed", nil, false},
	} {
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			doc := map[string]any{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/auth", "token_endpoint": srv.URL + "/token",
				"jwks_uri": srv.URL + "/jwks"}
			if tt.grants != nil {
				doc["grant_types_supported"] = tt.grants
			}
			_ = json.NewEncoder(w).Encode(doc)
		}))
		p, err := oidc.New(oidc.Config{Issuer: srv.URL, ClientID: "lajstrom", RedirectURL: "https://lajstrom.example/-/oidc/callback",
			AllowHTTP: true, Log: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		on := config.OIDC{Refresh: config.OIDCRefresh{Interval: time.Minute}}
		warnRefreshGrant(context.Background(), on, p, slog.New(slog.NewTextHandler(&logs, nil)))
		if got := strings.Contains(logs.String(), "lists no refresh_token grant"); got != tt.warn {
			t.Errorf("%s: warned %v, want %v: %s", tt.name, got, tt.warn, logs.String())
		}
		srv.Close()
	}
}
