package main

import (
	"context"
	"time"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// registryReport is the api.RegistryReporter of serve: each registry of the configuration, what
// its provider reports (provider.Reporter) and what the readiness ping of this replica knows of
// it, without a request to the registry.
type registryReport struct {
	cfgs      []config.Registry
	providers provider.Map
	health    *registryHealth
	replica   string
}

// Registries implements api.RegistryReporter.
func (r registryReport) Registries(context.Context) (string, []api.Registry) {
	out := make([]api.Registry, 0, len(r.cfgs))
	for _, c := range r.cfgs {
		reg := api.Registry{Name: c.Name, Type: c.Type, URL: c.URL, Prefix: c.Prefix}
		rep := provider.Report{Breaker: provider.BreakerClosed}
		if p, ok := r.providers[c.Name].(provider.Reporter); ok {
			rep = p.Report()
		}
		if rep.Vendor != "" {
			vendor, tier := rep.Vendor, api.SupportTier(rep.Vendor)
			reg.Vendor, reg.Support = &vendor, &tier
		}
		if rep.Capabilities != nil {
			reg.Capabilities = api.CapabilitiesOf(*rep.Capabilities)
		}
		reg.Status = registryStatus(r.health.status(c.Name), rep.Breaker, r.health.now())
		out = append(out, reg)
	}
	return r.replica, out
}

// registryStatus is the api.RegistryStatus of a registry whose last ping is s and whose breaker
// is b, at now: down when no ping was answered 200 within registryFresh, as /-/readyz counts it,
// or the breaker is open; degraded when the last ping failed or the breaker is half-open; ok
// otherwise. A registry that is not pinged counts as answering.
func registryStatus(s pingStatus, b provider.BreakerState, now time.Time) api.RegistryStatus {
	st := api.RegistryStatus{State: api.RegistryOK, Breaker: b}
	if !s.checkedAt.IsZero() {
		ms := s.took.Milliseconds()
		st.CheckedAt, st.LatencyMs = &s.checkedAt, &ms
	}
	if !s.answeredAt.IsZero() {
		st.AnsweredAt = &s.answeredAt
	}
	if s.err != "" {
		st.Message = &s.err
	}
	switch {
	case b == provider.BreakerOpen, s.pinged && (s.answeredAt.IsZero() || now.Sub(s.answeredAt) > registryFresh):
		st.State = api.RegistryDown
	case b == provider.BreakerHalfOpen, s.err != "":
		st.State = api.RegistryDegraded
	}
	return st
}
