package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The roles of docs/spec/rbac.md §3, beside RoleAdmin.
const (
	RoleReader     = "reader"
	RoleWriter     = "writer"
	RoleMaintainer = "maintainer"
)

// System is the format and the repo name under which the management API asks for the system
// resources of docs/spec/rbac.md §2 (users, bindings, audit, creating a repo): Allowed on
// System/System, or on any name with System in it, is the admin role and nothing else.
const System = "*"

// actionSet is a set of actions, one bit each.
type actionSet uint8

const (
	setRead actionSet = 1 << iota
	setWrite
	setDelete
	setManage
)

// actionBit is the bit of a; 0 for an action that is not one of the four.
func actionBit(a Action) actionSet {
	switch a {
	case ActionRead:
		return setRead
	case ActionWrite:
		return setWrite
	case ActionDelete:
		return setDelete
	case ActionManage:
		return setManage
	}
	return 0
}

// roleActions are the actions of each role on the repos its mask matches; admin has them on
// every repo, and the system besides.
var roleActions = map[string]actionSet{
	RoleReader:     setRead,
	RoleWriter:     setRead | setWrite,
	RoleMaintainer: setRead | setWrite | setDelete | setManage,
	RoleAdmin:      setRead | setWrite | setDelete | setManage,
}

// IsRole reports whether role is one of the four roles of docs/spec/rbac.md §3.
func IsRole(role string) bool {
	_, ok := roleActions[role]
	return ok
}

// repoKey is a repo, or a mask whose segments may be "*".
type repoKey struct{ format, name string }

// ParseMask splits a binding's mask <format>/<repo> into its two segments. Each must be "*" or
// not empty, without "/"; whether a segment names a format of this build or a valid repo name
// is for the caller (the bindings service).
func ParseMask(mask string) (format, repo string, err error) {
	format, repo, ok := strings.Cut(mask, "/")
	if !ok || format == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", fmt.Errorf("mask %q: want <format>/<repo>, each a name or *", mask)
	}
	return format, repo, nil
}

// RBAC is the Authorizer of docs/spec/rbac.md §3. It evaluates in memory, from a snapshot of
// the bindings and of the repos' anonymousRead, and reads the counter auth_version of the index
// before it evaluates a request: when the counter moved, it reloads the snapshot first, so a
// change of the bindings or of a repo counts from the next request on, on every replica. The
// evaluations of one request share that read when its context has an evaluation scope
// (WithEvaluationScope, which the server gives every request); without one, every evaluation
// reads the counter.
//
// The credential of the request (CredentialFrom) narrows the principal as in steps 2 and 3: a
// deploy token reaches its own repo with its scopes, a personal access token with scopes keeps
// those actions only. A user also has the bindings of its groups (docs/spec/rbac.md §8) until
// they lapse, by the clock at each evaluation. A binding that is not valid by §3 (a principal
// other than user:<name> or group:<name>, an unknown role, a mask that is not two segments)
// grants nothing and is logged at warn when the snapshot loads. Errors are reserved for an index that does not answer.
type RBAC struct {
	idx           index.Reader
	anonymousRead bool
	log           *slog.Logger
	now           func() time.Time

	mu   sync.Mutex // one reload at a time
	snap atomic.Pointer[snapshot]
}

var _ Authorizer = (*RBAC)(nil)

// RBACOptions configures NewRBAC.
type RBACOptions struct {
	// Index holds the bindings, the repos and auth_version; required.
	Index index.Reader
	// AnonymousRead is the server-wide switch auth.anonymousRead: false keeps every repo from
	// anonymous read, whatever its own anonymousRead.
	AnonymousRead bool
	// Log receives the reloads at debug and the bindings that grant nothing at warn; nil means
	// slog.Default().
	Log *slog.Logger
	// Now is the clock that a user's groups lapse by (docs/spec/rbac.md §8); nil means
	// time.Now.
	Now func() time.Time
}

// NewRBAC returns the evaluator over o.Index. It reads nothing before the first evaluation.
// NewRBAC panics when o.Index is nil.
func NewRBAC(o RBACOptions) *RBAC {
	if o.Index == nil {
		panic("auth: NewRBAC: nil Options.Index")
	}
	r := &RBAC{idx: o.Index, anonymousRead: o.AnonymousRead, log: o.Log, now: o.Now}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r
}

// Allowed reports whether p may perform a on format/repo, by the snapshot that is current for
// ctx, the credential of ctx and, for a user, its groups that have not lapsed.
func (r *RBAC) Allowed(ctx context.Context, p Principal, a Action, format, repo string) (bool, error) {
	s, err := r.current(ctx)
	if err != nil {
		return false, err
	}
	return s.allowed(p, a, repoKey{format, repo}, CredentialFrom(ctx), r.now()), nil
}

// snapshot is what RBAC evaluates from: the bindings, the repos with anonymous_read and the
// users' groups as of auth_version version.
type snapshot struct {
	version       int64
	anonymousRead bool                                // auth.anonymousRead
	admins        map[Principal]bool                  // the principals with an admin binding, users and groups
	grants        map[Principal]map[repoKey]actionSet // the other bindings, by principal and mask
	anon          map[repoKey]bool                    // the repos with anonymous_read
	groups        map[Principal][]membership          // the groups of each user, user:<uid>
}

// membership is a group of a user and when it lapses: the end of the login that brought it
// (docs/spec/rbac.md §8). A nil until never counts: the index always stores one with groups.
type membership struct {
	group Principal
	until *time.Time
}

// allowed is the evaluation of docs/spec/rbac.md §3 for p, narrowed by the credential c, with
// the groups of a user that have not lapsed at now.
func (s *snapshot) allowed(p Principal, a Action, repo repoKey, c Credential, now time.Time) bool {
	if actionBit(a) == 0 {
		return false
	}
	system := repo.format == System || repo.name == System
	switch p.Kind {
	case KindToken:
		// Step 2: its own repo, its scopes, never delete or manage (Credential.Limit).
		return !system && c.Kind == CredDeploy && strconv.FormatInt(c.TokenID, 10) == p.Name &&
			c.Limit(a, index.Repo{Format: repo.format, Name: repo.name}) == nil
	case KindUser:
		if allow, decided := s.user(p, a, repo, system, c, now); decided {
			return allow
		}
	case KindGroup, KindAnonymous:
		// A group is a principal of bindings, never of a request: it reaches a repo through its
		// members (step 4).
	}
	// Step 5, for every principal that got here; step 6.
	return !system && a == ActionRead && s.anonymousRead && s.anon[repo]
}

// user is steps 1, 3 and 4 for the user p, its own bindings and those of its groups that have
// not lapsed at now: decided is false when they leave a to step 5.
func (s *snapshot) user(p Principal, a Action, repo repoKey, system bool, c Credential, now time.Time) (allow, decided bool) {
	// Step 3: a personal access token with scopes keeps those actions, admin's included.
	if c.Kind == CredPAT && c.Limit(a, index.Repo{Format: repo.format, Name: repo.name}) != nil {
		return false, true
	}
	grants := func(q Principal) bool { // steps 1 and 4 for one principal
		return s.admins[q] || (!system && s.granted(q, repo)&actionBit(a) != 0)
	}
	if grants(p) {
		return true, true
	}
	for _, m := range s.groups[p] {
		if m.until != nil && now.Before(*m.until) && grants(m.group) {
			return true, true
		}
	}
	return false, false
}

// granted is the union of the actions of p's bindings whose mask matches repo: the exact mask,
// the format-wide one, the name across formats, and everything.
func (s *snapshot) granted(p Principal, repo repoKey) actionSet {
	g := s.grants[p]
	if g == nil {
		return 0
	}
	return g[repo] | g[repoKey{repo.format, "*"}] | g[repoKey{"*", repo.name}] | g[repoKey{"*", "*"}]
}

// evalScope is the snapshot that the evaluations of one request share.
type evalScope struct {
	mu    sync.Mutex
	owner *RBAC
	snap  *snapshot
}

// evalScopeKey is the context key of the request's evaluation scope.
type evalScopeKey struct{}

// WithEvaluationScope returns a copy of ctx in which the evaluations of an RBAC share one read
// of auth_version and one snapshot: the evaluations of one request, which may be many (a
// catalog checks every repo it lists). The server gives every request one; a request that
// evaluates nothing reads nothing.
func WithEvaluationScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, evalScopeKey{}, &evalScope{})
}

// current returns the snapshot of the request: the scope's when ctx has one that this RBAC
// filled, else a fresh one, which fills the scope.
func (r *RBAC) current(ctx context.Context) (*snapshot, error) {
	sc, ok := ctx.Value(evalScopeKey{}).(*evalScope)
	if !ok {
		return r.fresh(ctx)
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.snap != nil && sc.owner == r {
		return sc.snap, nil
	}
	s, err := r.fresh(ctx)
	if err != nil {
		return nil, err
	}
	sc.owner, sc.snap = r, s
	return s, nil
}

// fresh reads auth_version and returns the snapshot of that version, reloading it when the
// counter moved (or went back, a database restored).
func (r *RBAC) fresh(ctx context.Context) (*snapshot, error) {
	v, err := r.idx.AuthVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: rbac: %w", err)
	}
	s := r.snap.Load()
	if s != nil && s.version == v {
		return s, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.snap.Load(); cur != nil && cur != s {
		return cur, nil // another request reloaded it while this one waited
	}
	s, err = r.load(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: rbac: %w", err)
	}
	r.snap.Store(s)
	return s, nil
}

// load reads a snapshot: auth_version first, then the bindings, the repos and the users'
// groups, so that the snapshot is never older than the version it is kept under
// (index.Reader.AuthVersion).
func (r *RBAC) load(ctx context.Context) (*snapshot, error) {
	v, err := r.idx.AuthVersion(ctx)
	if err != nil {
		return nil, err
	}
	bindings, err := r.idx.ListBindings(ctx, "")
	if err != nil {
		return nil, err
	}
	repos, err := r.idx.ListRepos(ctx, "")
	if err != nil {
		return nil, err
	}
	members, err := r.idx.ListIdentityGroups(ctx)
	if err != nil {
		return nil, err
	}
	s := &snapshot{
		version:       v,
		anonymousRead: r.anonymousRead,
		admins:        map[Principal]bool{},
		grants:        map[Principal]map[repoKey]actionSet{},
		anon:          map[repoKey]bool{},
		groups:        map[Principal][]membership{},
	}
	for _, m := range members {
		u := Principal{Kind: KindUser, Name: m.UID}
		s.groups[u] = append(s.groups[u], membership{group: Principal{Kind: KindGroup, Name: m.Name}, until: m.ExpiresAt})
	}
	for _, b := range bindings {
		if err := s.add(b); err != nil {
			r.log.WarnContext(ctx, "binding grants nothing", "binding", b.ID, "principal", b.Principal,
				"role", b.Role, "mask", b.RepoMask, "reason", err.Error())
		}
	}
	for _, repo := range repos {
		if repo.AnonymousRead {
			s.anon[repoKey{repo.Format, repo.Name}] = true
		}
	}
	r.log.DebugContext(ctx, "rbac snapshot loaded", "authVersion", v, "bindings", len(bindings),
		"anonymousRepos", len(s.anon), "groupMembers", len(members))
	return s, nil
}

// errBinding means a binding is not valid by docs/spec/rbac.md §3.
var errBinding = errors.New("not a valid binding")

// add puts the binding b into s, or says why it grants nothing.
func (s *snapshot) add(b index.Binding) error {
	p, err := ParsePrincipal(b.Principal)
	switch {
	case err != nil:
		return err
	case p.Kind == KindGroup:
		if err := CheckGroupName(p.Name); err != nil {
			return fmt.Errorf("%w: %w", errBinding, err)
		}
	case p.Kind != KindUser:
		return fmt.Errorf("%w: the principal of a binding is user:<name> or group:<name>", errBinding)
	}
	actions, ok := roleActions[b.Role]
	if !ok {
		return fmt.Errorf("%w: unknown role %q", errBinding, b.Role)
	}
	format, repo, err := ParseMask(b.RepoMask)
	if err != nil {
		return fmt.Errorf("%w: %w", errBinding, err)
	}
	if b.Role == RoleAdmin {
		s.admins[p] = true
		return nil
	}
	g := s.grants[p]
	if g == nil {
		g = map[repoKey]actionSet{}
		s.grants[p] = g
	}
	g[repoKey{format, repo}] |= actions
	return nil
}
