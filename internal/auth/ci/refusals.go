package ci

import (
	"cmp"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/server"
)

// The outcome of the token.exchange row of an exchange (docs/spec/rbac.md §11).
const (
	outcomeMinted  = "minted"
	outcomeRefused = "refused"
)

// The rate of the audit rows of refusals, per replica (docs/spec/rbac.md §11).
const (
	// refusalEvery is how often at most a key gets a row.
	refusalEvery = time.Minute
	// maxRefusalKeys bounds the keys kept; a refusal of a key beyond them writes no row.
	maxRefusalKeys = 10000
	// refusalForget is how long a key whose count was never carried is kept.
	refusalForget = time.Hour
)

// refusal is a refused subject token: the description the client gets, the reason of the log and
// the row when it is not the description, the issuer its iss named, and the claims of a token
// that verified.
type refusal struct {
	description, reason string
	issuer              *Issuer
	claims              map[string]any
}

// refuseToken answers the refusal f with 400 invalid_grant, logs it at info, and writes its
// token.exchange row of outcome refused unless the rate of its key holds it back (refusals).
func (e *Exchange) refuseToken(w http.ResponseWriter, r *http.Request, f refusal) {
	ctx := r.Context()
	reason := f.description
	if f.reason != "" {
		reason += ": " + f.reason
	}
	details := map[string]any{"outcome": outcomeRefused, "reason": reason}
	if f.issuer != nil {
		details["issuer"] = f.issuer.Name
	}
	p, project, key, claims := f.who(clientOf(r))
	if claims != nil {
		details["claims"] = claims
	}
	e.log().InfoContext(ctx, "ci: token exchange refused", "error", "invalid_grant", "reason", reason, "project", project,
		"address", clientOf(r), "requestId", server.RequestID(ctx))
	if ok, suppressed := e.refusals.take(key, e.now()); ok {
		if suppressed > 0 {
			details["suppressed"] = suppressed
		}
		if err := e.appendRow(ctx, r, p, details); err != nil {
			e.log().ErrorContext(ctx, "ci: the audit row of a refusal not written", "error", err.Error(), "requestId", server.RequestID(ctx))
		}
	}
	writeOAuthError(w, http.StatusBadRequest, "invalid_grant", f.description)
}

// who returns what the row of f tells of the token: by the ci: principal of a token that verified
// and whose project keeps the grammar, anonymous otherwise; its project; the key of the rate, the
// issuer and the project's id (else its name) of a token that verified, the client address addr
// otherwise; and the claims of a token that verified that say who and where (ids, project, ref,
// ref_type, environment, event), nil for one that did not.
func (f refusal) who(addr string) (p auth.Principal, project, key string, claims map[string]string) {
	if f.issuer == nil || f.claims == nil {
		return auth.Anonymous, "", "address " + addr, nil
	}
	pf := f.issuer.pf
	project, _ = f.claims[pf.Project].(string)
	p = auth.Anonymous
	if ci, err := auth.CIPrincipal(f.issuer.Name, project); err == nil {
		p = ci
	}
	claims = map[string]string{}
	told := append([]string{pf.Project, pf.Event, "ref", "ref_type", "environment"}, pf.IDs...)
	for c, v := range f.claims {
		if s, ok := claimText(v); ok && slices.Contains(told, c) {
			claims[c] = s[:min(len(s), maxAuditClaim)]
		}
	}
	key = "project " + f.issuer.Name + "/" + cmp.Or(claims[pf.IDs[0]], project)
	return p, project, key, claims
}

// refusals is the rate of the audit rows of refusals on a replica: one row a minute per key, the
// refusals held back counted and carried by the key's next row. Its zero value is ready.
type refusals struct {
	mu   sync.Mutex
	keys map[string]*refusalWindow
}

// refusalWindow is a key's last row and the refusals of the key held back since.
type refusalWindow struct {
	last       time.Time
	suppressed int
}

// take reports whether a refusal of key at now gets a row, and how many refusals of the key the
// row carries as held back. With maxRefusalKeys keys kept, the windows that closed with nothing
// held back, and those older than refusalForget, make room; without room the refusal gets no row.
func (r *refusals) take(key string, now time.Time) (bool, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keys == nil {
		r.keys = map[string]*refusalWindow{}
	}
	if w, ok := r.keys[key]; ok {
		if now.Sub(w.last) < refusalEvery {
			w.suppressed++
			return false, 0
		}
		held := w.suppressed
		w.last, w.suppressed = now, 0
		return true, held
	}
	if len(r.keys) >= maxRefusalKeys {
		for k, w := range r.keys {
			if age := now.Sub(w.last); (age >= refusalEvery && w.suppressed == 0) || age >= refusalForget {
				delete(r.keys, k)
			}
		}
		if len(r.keys) >= maxRefusalKeys {
			return false, 0
		}
	}
	r.keys[key] = &refusalWindow{last: now}
	return true, 0
}
