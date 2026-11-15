package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// RegistryReporter says what GET /api/v1/registries answers of the registries of the
// configuration but the repos on each, which the API reads in the index: serve implements it
// over the configuration, the providers (provider.Reporter) and its readiness ping, without a
// request to a registry.
type RegistryReporter interface {
	// Registries returns the replica that answers, as replicas.id names it, and the registries
	// as it knows them.
	Registries(ctx context.Context) (replica string, registries []Registry)
}

// RegistryList is the answer of GET /api/v1/registries.
type RegistryList struct {
	Replica    string     `json:"replica"`
	Registries []Registry `json:"registries"`
}

// Registry is a registry of the configuration as the admin reads it (docs/api.md, Registries):
// never Lajstrom's credentials for it.
type Registry struct {
	Name   string `json:"name"`
	Type   string `json:"type"` // generic or ecr
	URL    string `json:"url"`
	Prefix string `json:"prefix"`
	// Capabilities are those ingest and the facade go by; nil before a probe of the replica
	// succeeded.
	Capabilities *Capabilities `json:"capabilities"`
	// Repos are the repos on it, <format>/<name>, by format and name; the API fills them.
	Repos  []string       `json:"repos"`
	Status RegistryStatus `json:"status"`
}

// Capabilities are a registry's provider.Capabilities, under the keys of
// registries[].capabilities (docs/configuration.md, Registry capabilities).
type Capabilities struct {
	CreateOnPush       bool   `json:"createOnPush"`
	Referrers          bool   `json:"referrers"`
	Catalog            bool   `json:"catalog"`
	DeleteManifest     bool   `json:"deleteManifest"`
	ImmutableTags      bool   `json:"immutableTags"`
	BlobRedirect       bool   `json:"blobRedirect"`
	ReferrersAlwaysTag bool   `json:"referrersAlwaysTag"`
	MaxBlobSize        int64  `json:"maxBlobSize"`
	Consistency        string `json:"consistency"`      // strong, eventual, or "" when unknown
	ConsistencyBound   string `json:"consistencyBound"` // a duration: "2m10s", "0s" when none is known
	MaxNameDepth       int    `json:"maxNameDepth"`
	MaxNameLength      int    `json:"maxNameLength"`
	TagsFoldCase       bool   `json:"tagsFoldCase"`
	BuildRoutes        bool   `json:"buildRoutes"`
}

// CapabilitiesOf returns the API form of c.
func CapabilitiesOf(c provider.Capabilities) *Capabilities {
	return &Capabilities{CreateOnPush: c.CreateOnPush, Referrers: c.Referrers, Catalog: c.Catalog, DeleteManifest: c.DeleteManifest,
		ImmutableTags: c.ImmutableTags, BlobRedirect: c.BlobRedirect, ReferrersAlwaysTag: c.ReferrersAlwaysTag,
		MaxBlobSize: c.MaxBlobSize, Consistency: string(c.Consistency), ConsistencyBound: c.ConsistencyBound.String(),
		MaxNameDepth: c.MaxNameDepth, MaxNameLength: c.MaxNameLength, TagsFoldCase: c.TagsFoldCase, BuildRoutes: c.BuildRoutes}
}

// RegistryStatus is what the replica's readiness ping and the registry's circuit breaker know.
type RegistryStatus struct {
	// State is RegistryDown, RegistryDegraded or RegistryOK.
	State string `json:"state"`
	// CheckedAt is the time of the last ping, AnsweredAt of the last one answered 200; nil
	// before the first, and CheckedAt for a provider that is not pinged.
	CheckedAt  *time.Time `json:"checkedAt"`
	AnsweredAt *time.Time `json:"answeredAt"`
	// LatencyMs is how long the last ping took, in milliseconds; nil before the first.
	LatencyMs *int64 `json:"latencyMs"`
	// Message is the error of the last ping when it failed, as the provider words it; nil
	// otherwise.
	Message *string               `json:"message"`
	Breaker provider.BreakerState `json:"breaker"`
}

// The states of a registry (docs/api.md, Registries).
const (
	// RegistryDown: no ping was answered 200 for a minute, or the breaker is open.
	RegistryDown = "down"
	// RegistryDegraded: the last ping failed, or the breaker is half-open.
	RegistryDegraded = "degraded"
	// RegistryOK: neither.
	RegistryOK = "ok"
)

// listRegistries answers the registries of the configuration as the replica knows them, each
// with the repos on it, to the admin role, asked before anything else (docs/spec/rbac.md §3).
// Without a RegistryReporter it answers none.
func (h *handler) listRegistries(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	out := RegistryList{Registries: []Registry{}}
	if h.registries != nil {
		out.Replica, out.Registries = h.registries.Registries(r.Context())
	}
	repos, err := h.repos.Index.ListRepos(r.Context(), "")
	if err != nil {
		return 0, nil, err
	}
	for i := range out.Registries {
		reg := &out.Registries[i]
		reg.Repos = []string{}
		for _, repo := range repos {
			if repo.Registry == reg.Name {
				reg.Repos = append(reg.Repos, repo.Format+"/"+repo.Name)
			}
		}
	}
	slices.SortFunc(out.Registries, func(a, b Registry) int { return strings.Compare(a.Name, b.Name) })
	return http.StatusOK, out, nil
}
