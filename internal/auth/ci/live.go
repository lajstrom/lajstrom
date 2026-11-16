package ci

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Source gives the trust policies that count now: Live, or a *Policies of the configuration alone.
type Source interface {
	Current(ctx context.Context) (*Policies, error)
}

// Current returns ps itself: a set of policies that never changes.
func (ps *Policies) Current(context.Context) (*Policies, error) { return ps, nil }

// Live is the trust policies of auth.ci and of the API as they are now, on every replica
// (docs/spec/rbac.md §11): the configuration's, read at start, and the index's that count, read
// again when auth_version moved, which every write of ci_policies moves in its transaction, and
// when the first expiry among them passed. Create it with NewLive; its methods may be called
// concurrently.
type Live struct {
	cfg config.CI
	idx index.Reader
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	version int64
	cur     *Policies // nil until the first read
	next    time.Time // the first expiry of a policy of cur; zero for none
}

// NewLive returns the policies of c and of the index idx. It reads nothing from the index: the
// first Current does.
func NewLive(c config.CI, idx index.Reader, log *slog.Logger) (*Live, error) {
	if _, err := New(c); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Live{cfg: c, idx: idx, log: log, now: time.Now}, nil
}

// Current returns the policies of the configuration and of the index as of the auth_version it
// reads first, reading ci_policies again when the counter moved since the last read, or when the
// first expiry among the policies it took passed. A row that is not active (pending, expired), or
// whose expiry passed, does not count; one whose name a policy of the configuration has, that
// breaks a rule of a policy, or that writes without an expiry or outside auth.ci.apiWriteRepos,
// is left out and logged at warn. An index that does not answer is an error: no policy of the API
// is taken for granted.
func (l *Live) Current(ctx context.Context) (*Policies, error) {
	v, err := l.idx.AuthVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("ci: the trust policies: %w", err)
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur != nil && l.version == v && (l.next.IsZero() || now.Before(l.next)) {
		return l.cur, nil
	}
	rows, err := l.idx.ListCIPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("ci: the trust policies of the API: %w", err)
	}
	c := l.cfg
	c.Policies = slices.Clone(l.cfg.Policies)
	var next time.Time
	for _, r := range rows {
		if r.State != index.CIPolicyActive || expired(r, now) {
			continue
		}
		p := configPolicy(r)
		if reason := l.leaveOut(p, r.ExpiresAt); reason != "" {
			l.log.WarnContext(ctx, "CI trust policy of the API left out", "policy", r.Name, "reason", reason)
			continue
		}
		if r.ExpiresAt != nil && (next.IsZero() || r.ExpiresAt.Before(next)) {
			next = *r.ExpiresAt
		}
		c.Policies = append(c.Policies, p)
	}
	ps, err := New(c)
	if err != nil {
		return nil, err
	}
	l.cur, l.version, l.next = ps, v, next
	return ps, nil
}

// leaveOut says why the policy p of the API, which expires at expiresAt, does not count, or "".
func (l *Live) leaveOut(p config.CIPolicy, expiresAt *time.Time) string {
	if slices.ContainsFunc(l.cfg.Policies, func(c config.CIPolicy) bool { return c.Name == p.Name }) {
		return "a policy of the configuration has its name"
	}
	if errs := config.CheckCIPolicy(l.cfg.Issuers, p); len(errs) > 0 {
		return errs[0].Error()
	}
	if !writes(p) {
		return ""
	}
	if expiresAt == nil {
		return "it grants writer or maintainer, and has no expiry"
	}
	for _, g := range p.Grants {
		if config.CIWrites(g.Role) && !l.cfg.WriteReposAllow(g.Mask) {
			return fmt.Sprintf("it grants %s on %s, outside auth.ci.apiWriteRepos", g.Role, g.Mask)
		}
	}
	return ""
}

// configPolicy is the row r as a policy of the configuration.
func configPolicy(r index.CIPolicy) config.CIPolicy {
	p := config.CIPolicy{Name: r.Name, Issuer: r.Issuer, Claims: map[string]config.CIValues{}}
	for c, v := range r.Claims {
		p.Claims[c] = config.CIValues(v)
	}
	for _, g := range r.Grants {
		p.Grants = append(p.Grants, config.CIGrant{Role: g.Role, Mask: g.Mask})
	}
	return p
}

// writes reports whether p grants writer or maintainer.
func writes(p config.CIPolicy) bool {
	return slices.ContainsFunc(p.Grants, func(g config.CIGrant) bool { return config.CIWrites(g.Role) })
}
