package main

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// The registry probe of /-/readyz (docs/spec/http-routing.md §5): every registry answered
// GET /v2/ within registryFresh. It pings every registryPingEvery, each ping bounded by
// registryProbeTimeout.
const (
	registryFresh     = time.Minute
	registryPingEvery = 20 * time.Second
)

// registryHealth pings the registries in the background and remembers when each last answered,
// and which refused Lajstrom's credentials at their last ping. A registry whose provider is not
// a provider.Pinger counts as answering.
type registryHealth struct {
	pingers map[string]provider.Pinger
	log     *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	lastOK  map[string]time.Time
	failed  map[string]bool // logged once per outage, and again when its kind changes
	refused map[string]bool // the last ping was provider.ErrUnauthorized: a 401 or 403
}

// newRegistryHealth returns the probe of providers; it pings nothing until Ping or Run.
func newRegistryHealth(providers provider.Map, log *slog.Logger) *registryHealth {
	h := &registryHealth{pingers: map[string]provider.Pinger{}, log: log, now: time.Now,
		lastOK: map[string]time.Time{}, failed: map[string]bool{}, refused: map[string]bool{}}
	for name, p := range providers {
		if pinger, ok := p.(provider.Pinger); ok {
			h.pingers[name] = pinger
		}
	}
	return h
}

// Ping pings every registry once, at the same time, and records the answers. A registry that
// refuses Lajstrom's credentials answers, so it is not logged as one that does not.
func (h *registryHealth) Ping(ctx context.Context) {
	var wg sync.WaitGroup
	for name, p := range h.pingers {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, registryProbeTimeout)
			defer cancel()
			err := p.Ping(ctx)
			h.mu.Lock()
			defer h.mu.Unlock()
			switch {
			case err == nil:
				if h.failed[name] {
					h.log.InfoContext(ctx, "registry answers again", "registry", name)
				}
				h.lastOK[name], h.failed[name], h.refused[name] = h.now(), false, false
			case errors.Is(err, provider.ErrUnauthorized):
				if !h.failed[name] || !h.refused[name] {
					h.log.WarnContext(ctx, "registry refuses Lajstrom's credentials at GET /v2/", "registry", name, "error", err.Error())
				}
				h.failed[name], h.refused[name] = true, true
			case ctx.Err() == nil || errors.Is(err, context.DeadlineExceeded):
				if !h.failed[name] || h.refused[name] {
					h.log.WarnContext(ctx, "registry does not answer GET /v2/", "registry", name, "error", err.Error())
				}
				h.failed[name], h.refused[name] = true, false
			}
		})
	}
	wg.Wait()
}

// Run pings every registryPingEvery until ctx ends.
func (h *registryHealth) Run(ctx context.Context) {
	t := time.NewTicker(registryPingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.Ping(ctx)
		}
	}
}

// Stale returns the registries that have not answered within registryFresh, sorted.
func (h *registryHealth) Stale() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var stale []string
	for name := range h.pingers {
		if last, ok := h.lastOK[name]; !ok || h.now().Sub(last) > registryFresh {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	return stale
}

// Refused reports whether a registry of names refused Lajstrom's credentials at its last ping.
func (h *registryHealth) Refused(names []string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, name := range names {
		if h.refused[name] {
			return true
		}
	}
	return false
}
