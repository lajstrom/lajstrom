package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// newOutbox is the outbox of the events of serve's writes and of the admin commands' (ingest,
// api.Repos): their webhook deliveries in the transaction of each audit row, with
// server.externalURL the base of every event's source (docs/spec/webhooks.md §2).
func newOutbox(c *config.Config) *webhook.Outbox {
	return &webhook.Outbox{Source: c.Server.ExternalURL}
}

// newDeliverer is the Deliverer of webhooks.* of the configuration: the allowed hosts and the
// timeout of an attempt.
func newDeliverer(c *config.Config, log *slog.Logger) *webhook.Deliverer {
	return webhook.NewDeliverer(webhook.Options{AllowedHosts: c.Webhooks.AllowedHosts, Timeout: c.Webhooks.Timeout, Log: log})
}

// startWebhooks starts this replica's worker of the webhooks' deliveries on wg, under the
// replica's id, until ctx ends (docs/spec/webhooks.md §4). It starts none when webhooks.workers
// is 0, and none without a master key, which every webhook's secret needs: the events wait in
// the index for a replica that has one. A master key that is configured and does not load has
// stopped serve before, at newSigner; were it to fail here, delivery is off, logged at error.
func startWebhooks(ctx context.Context, c *config.Config, lookupEnv func(string) (string, bool), st index.Store, replica string,
	wg *sync.WaitGroup, log *slog.Logger) {
	if c.Webhooks.Workers == 0 {
		log.Info("webhook delivery off on this replica (webhooks.workers is 0)")
		return
	}
	m, err := loadMaster(c.Keys, lookupEnv, nil)
	switch {
	case errors.Is(err, keys.ErrNoMasterKey):
		log.Info("webhook delivery off: webhooks need the master key (keys.masterKeyEnv or keys.masterKeyFile)")
		return
	case err != nil:
		log.Error("webhook delivery off: the master key does not load", "error", err.Error())
		return
	}
	w := &webhook.Worker{Index: st, Deliverer: newDeliverer(c, log), Master: m, Replica: replica, Workers: c.Webhooks.Workers,
		MaxAttempts: c.Webhooks.MaxAttempts, Retention: c.Webhooks.Retention, Log: log}
	wg.Go(func() { w.Run(ctx) })
	log.Info("webhook delivery on", "webhooks.workers", c.Webhooks.Workers, "webhooks.allowedHosts", len(c.Webhooks.AllowedHosts),
		"webhooks.timeout", c.Webhooks.Timeout.String())
}
