package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// hookStore is a migrated SQLite store with one active webhook of raw/* to target, its secret
// sealed with the master key of hex master.
func hookStore(t *testing.T, master, target string) (*sqlite.Store, index.Webhook) {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:"+filepath.Join(t.TempDir(), "hooks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m, err := keys.ParseMaster(master)
	if err != nil {
		t.Fatal(err)
	}
	_, key := webhook.NewSecret()
	h := index.Webhook{UID: index.NewUID(time.Now()), Mask: "raw/*", URL: target, Active: true, CreatedBy: "user:admin"}
	if h.SecretEnc, err = webhook.Seal(m, h.UID, key); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWebhook(ctx, &h); err != nil {
		t.Fatal(err)
	}
	return st, h
}

// TestStartWebhooks: a repo created through the repo service of serve and of admin repo writes
// its repo.created in the outbox, and the worker that startWebhooks starts with the master key
// delivers it to a host of webhooks.allowedHosts, signed; without workers or without a master key
// no worker starts, and that is logged.
func TestStartWebhooks(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got := make(chan *http.Request, 4)
	rec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(rec.Close)
	master := strings.Repeat("0123456789abcdef", 4)
	st, h := hookStore(t, master, rec.URL)
	env := func(k string) (string, bool) { return master, k == "LAJSTROM_MASTER_KEY" }
	c := config.Default()
	c.Server.ExternalURL = "https://art.example.com"
	c.Registries = []config.Registry{{Name: "local"}}
	c.Keys.MasterKeyEnv = "LAJSTROM_MASTER_KEY"
	c.Webhooks.AllowedHosts = []string{"127.0.0.1"}

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	var wg sync.WaitGroup
	noWorkers := *c
	noWorkers.Webhooks.Workers = 0
	startWebhooks(ctx, &noWorkers, env, st, "replica-1", &wg, log)
	startWebhooks(ctx, c, func(string) (string, bool) { return "", false }, st, "replica-1", &wg, log)
	for _, want := range []string{"webhooks.workers is 0", "webhooks need the master key"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not say %q:\n%s", want, logs.String())
		}
	}

	if _, err := newRepos(c, st, log, nil).Create(ctx, api.CreateRequest{Format: "raw", Name: "scratch", Registry: "local"},
		api.Origin{Source: api.SourceCLI}); err != nil {
		t.Fatal(err)
	}
	startWebhooks(ctx, c, env, st, "replica-1", &wg, log)
	select {
	case r := <-got:
		if r.Header.Get("webhook-id") == "" || !strings.HasPrefix(r.Header.Get("webhook-signature"), "v1,") {
			t.Errorf("headers %v", r.Header)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no delivery 10 s after the worker started; log:\n%s", logs.String())
	}
	cancel()
	wg.Wait()
	ds, err := st.ListWebhookDeliveries(t.Context(), h.ID, 0, 10)
	if err != nil || len(ds) != 1 || ds[0].EventType != webhook.TypeRepoCreated {
		t.Errorf("deliveries %+v, %v; want the repo.created", ds, err)
	}
}
