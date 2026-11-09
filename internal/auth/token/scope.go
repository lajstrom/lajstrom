package token

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Types of scopes and access entries.
const (
	// TypeRepository is a repository: its name is the facade's <format>/<repo>/<path>.
	TypeRepository = "repository"
	// TypeRegistry is the registry itself; only registry:catalog:* is kept, which clients ask
	// for to list /v2/_catalog.
	TypeRegistry = "registry"
	// catalogName is the name of the registry scope of the catalog.
	catalogName = "catalog"
)

// The actions of a repository scope (docs/spec/rbac.md §2: pull is read, push is write).
const (
	ActionPull   = "pull"
	ActionPush   = "push"
	ActionDelete = "delete"
)

// Access is a scope a client asks for, and an entry of the access claim of a token: a type, a
// name and actions.
type Access struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// CatalogScope is the scope of /v2/_catalog, for the challenge of a 401 there.
const CatalogScope = TypeRegistry + ":" + catalogName + ":*"

// maxNameLen bounds a repository name: clients limit host, port and name together to 255
// bytes.
const maxNameLen = 255

// nameGrammar is the grammar of a repository name (distribution spec, "Pulling manifests"), the
// facade's.
var nameGrammar = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)

// ValidName reports whether name is a repository name of the facade: the grammar of the
// distribution spec, at most 255 bytes, and at least three components, <format>/<repo>/<path>.
func ValidName(name string) bool {
	return len(name) <= maxNameLen && strings.Count(name, "/") >= 2 && nameGrammar.MatchString(name)
}

// ParseScopes parses the scope parameters of a token request, each
// repository:<name>:<actions>, several in one parameter separated by spaces as OAuth2 has them.
// The actions are a comma list of pull, push and delete, * meaning the three; any other action
// is ignored. A scope of another type, or whose name is not ValidName, is dropped, as the
// distribution token flow expects, but for registry:catalog:*, which is kept. A repository
// asked for more than once gets the union of the actions; one with no action left is dropped.
// The result is sorted by type and name, the actions in the order pull, push, delete.
func ParseScopes(params []string) []Access {
	var sc scopeSet
	for _, param := range params {
		for _, s := range strings.Fields(param) {
			sc.add(s)
		}
	}
	return sc.access()
}

// scopeSet collects the scopes of a token request.
type scopeSet struct {
	catalog bool
	repos   map[string]map[string]bool // name → actions
}

// add parses one scope, type:name:actions, and keeps what ParseScopes keeps.
func (sc *scopeSet) add(s string) {
	typ, rest, _ := strings.Cut(s, ":")
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return
	}
	name, actions := rest[:i], strings.Split(rest[i+1:], ",")
	switch {
	case typ == TypeRegistry && name == catalogName && slices.Contains(actions, "*"):
		sc.catalog = true
	case typ == TypeRepository && ValidName(name):
		if sc.repos == nil {
			sc.repos = map[string]map[string]bool{}
		}
		set := sc.repos[name]
		if set == nil {
			set = map[string]bool{}
			sc.repos[name] = set
		}
		for _, a := range actions {
			switch a {
			case "*":
				set[ActionPull], set[ActionPush], set[ActionDelete] = true, true, true
			case ActionPull, ActionPush, ActionDelete:
				set[a] = true
			}
		}
	}
}

// access returns the scopes collected, sorted by type and name, the actions in the order pull,
// push, delete; a repository with no action is left out.
func (sc *scopeSet) access() []Access {
	var out []Access
	if sc.catalog {
		out = append(out, Access{Type: TypeRegistry, Name: catalogName, Actions: []string{"*"}})
	}
	for _, name := range slices.Sorted(maps.Keys(sc.repos)) {
		var actions []string
		for _, a := range []string{ActionPull, ActionPush, ActionDelete} {
			if sc.repos[name][a] {
				actions = append(actions, a)
			}
		}
		if len(actions) > 0 {
			out = append(out, Access{Type: TypeRepository, Name: name, Actions: actions})
		}
	}
	return out
}

// Reduce returns the scopes with the actions allowed to p on each repository's
// <format>/<repo> (pull is read, push write, delete delete). A scope with no action left stays,
// with none (docs/spec/rbac.md §4): the token then names every repository it was asked for, and
// whether one was granted nothing because it may not be read or because it does not exist is
// not in it. The credential of the request (auth.CredentialFrom(ctx)) decides first, as
// format.Authorize has it: a personal access token with scopes keeps only the actions of its
// scopes, a deploy token only those of its scopes on its own repo, neither ever delete; so the
// token carries the narrowing to every /v2 request. authz decides next, by the bindings
// (auth.RBAC); a nil authz allows what the credential allows, for the tests. The catalog scope
// is kept for every principal but a personal access token whose scopes do not read: the
// catalog is filtered repo by repo when it is listed, by the principal's bindings, and a /v2
// request no longer carries the credential that would narrow them (#63, T-9 of the threat
// model). An error of authz is returned: the caller cannot decide.
func Reduce(ctx context.Context, authz auth.Authorizer, p auth.Principal, scopes []Access) ([]Access, error) {
	cred := auth.CredentialFrom(ctx)
	out := make([]Access, 0, len(scopes))
	for _, s := range scopes {
		if s.Type != TypeRepository {
			if cred.Kind == auth.CredPAT && cred.Limit(auth.ActionRead, index.Repo{}) != nil {
				continue // a token that may read nothing lists nothing
			}
			out = append(out, Access{Type: s.Type, Name: s.Name, Actions: slices.Clone(s.Actions)})
			continue
		}
		format, rest, _ := strings.Cut(s.Name, "/")
		repo, _, _ := strings.Cut(rest, "/")
		kept := []string{}
		for _, a := range s.Actions {
			if cred.Limit(authAction(a), index.Repo{Format: format, Name: repo}) != nil {
				continue // outside the credential's scopes or repo: never granted
			}
			ok := authz == nil
			if !ok {
				var err error
				if ok, err = authz.Allowed(ctx, p, authAction(a), format, repo); err != nil {
					return nil, fmt.Errorf("token: may %s %s %s/%s: %w", p, a, format, repo, err)
				}
			}
			if ok {
				kept = append(kept, a)
			}
		}
		out = append(out, Access{Type: s.Type, Name: s.Name, Actions: kept})
	}
	return out, nil
}

// authAction is the RBAC action of a scope action.
func authAction(a string) auth.Action {
	switch a {
	case ActionPush:
		return auth.ActionWrite
	case ActionDelete:
		return auth.ActionDelete
	}
	return auth.ActionRead
}

// ScopeAction is the scope action of an RBAC action: pull for read, push for write, delete for
// delete; manage has none, and no token grants it.
func ScopeAction(a auth.Action) string {
	switch a {
	case auth.ActionRead:
		return ActionPull
	case auth.ActionWrite:
		return ActionPush
	case auth.ActionDelete:
		return ActionDelete
	case auth.ActionManage:
	}
	return ""
}

// Realm returns the realm and the service of the token flow for r (docs/spec/http-routing.md
// §1). With externalURL, server.externalURL, the realm is <scheme>://<host>/v2/token and the
// service the host, with the port when it has one. Without, the realm is
// http://<Host>/v2/token and the service the request's Host: the dev stack. Replicas behind
// one edge share one Host, so a token from one is for the service of the other.
func Realm(externalURL string, r *http.Request) (realm, service string) {
	if u, err := url.Parse(externalURL); externalURL != "" && err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + "/v2/token", u.Host
	}
	return "http://" + r.Host + "/v2/token", r.Host
}

// Challenge returns the WWW-Authenticate value of a 401 under /v2/:
// Bearer realm="<realm>",service="<service>", then scope="<scope>" when scope is not empty
// (several scopes separated by spaces) and error="insufficient_scope" when a valid token
// lacked the scope.
func Challenge(realm, service, scope string, insufficient bool) string {
	var b strings.Builder
	b.WriteString("Bearer realm=" + strconv.Quote(realm) + ",service=" + strconv.Quote(service))
	if scope != "" {
		b.WriteString(",scope=" + strconv.Quote(scope))
	}
	if insufficient {
		b.WriteString(`,error="insufficient_scope"`)
	}
	return b.String()
}
