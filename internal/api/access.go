package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/ci"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The access report (docs/spec/rbac.md §14, #1279): who has what at one moment, joined per
// principal, in one JSON document of a canonical form, so that the same rows at the same time
// are the same bytes from GET /api/v1/access-report and from lajstrom admin access-report, which
// both build it here. It holds no secret and no hash, but a token's hashPrefix as the tokens API
// answers it.

// The bounds of unusedDays: after how many days without a use a user or a token is unused, 90
// by default, PCI DSS 8.2.6.
const (
	DefaultUnusedDays = 90
	MinUnusedDays     = 1
	MaxUnusedDays     = 3650
)

// The flags of the access report (docs/spec/rbac.md §14), what a reviewer looks for; a list of
// them is in this order.
const (
	FlagAdmin              = "admin"
	FlagAllRepos           = "allRepos"
	FlagAdminWithoutEffect = "adminWithoutEffect"
	FlagNoExpiry           = "noExpiry"
	FlagUnused             = "unused"
	FlagBeyondMaxTTL       = "beyondMaxTTL"
	FlagOwnerDisabled      = "ownerDisabled"
	FlagInactive           = "inactive"
)

// AccessConfig is what the access report reads of the configuration: the settings its flags
// read, and the CI trust policies of the configuration. serve and the command line make it of
// the same keys, so that they answer the same document.
type AccessConfig struct {
	// AdminMFA is auth.admin.requireMFA, AnonymousRead auth.anonymousRead.
	AdminMFA      bool
	AnonymousRead bool
	// InactiveAfter is auth.disableInactiveUsersAfter, Lockout auth.lockout.duration (0 while
	// local passwords are off), as auth.Users has them.
	InactiveAfter time.Duration
	Lockout       time.Duration
	// DeployMaxTTL is auth.deployTokens.maxTTL, ServiceMaxTTL auth.serviceAccounts.maxTokenTTL,
	// as auth.Tokens has them.
	DeployMaxTTL  time.Duration
	ServiceMaxTTL time.Duration
	// CIPolicies are auth.ci.policies.
	CIPolicies []config.CIPolicy
}

// AccessQuery is what a report covers: Scope, auth.AccessAll, auth.AccessPeople or
// auth.AccessSystems, and UnusedDays, MinUnusedDays to MaxUnusedDays.
type AccessQuery struct {
	Scope      string
	UnusedDays int
}

// AccessReport is the document of the access report (docs/spec/rbac.md §14). A section that its
// scope leaves out is nil, and absent from the document; anonymous is absent while
// auth.anonymousRead is off.
type AccessReport struct {
	GeneratedAt  time.Time         `json:"generatedAt"`
	Scope        string            `json:"scope"`
	UnusedDays   int               `json:"unusedDays"`
	Settings     AccessSettings    `json:"settings"`
	Users        []AccessUser      `json:"users"`
	Groups       *[]AccessGroup    `json:"groups,omitempty"`
	Workloads    *[]AccessWorkload `json:"workloads,omitempty"`
	DeployTokens *[]AccessToken    `json:"deployTokens,omitempty"`
	CIPolicies   *[]AccessCIPolicy `json:"ciPolicies,omitempty"`
	Anonymous    *AccessAnonymous  `json:"anonymous,omitempty"`
}

// AccessSettings are the keys of the configuration that the flags read, the durations in seconds
// (0: off).
type AccessSettings struct {
	AdminMFA                   bool  `json:"adminMFA"`
	AnonymousRead              bool  `json:"anonymousRead"`
	DisableInactiveUsersAfter  int64 `json:"disableInactiveUsersAfter"`
	DeployTokensMaxTTL         int64 `json:"deployTokensMaxTTL"`
	ServiceAccountsMaxTokenTTL int64 `json:"serviceAccountsMaxTokenTTL"`
}

// AccessUser is a live user as the users API answers it, with its principal, its current groups,
// its bindings, its own and its groups', its live personal access tokens and its flags.
type AccessUser struct {
	auth.User
	Principal string             `json:"principal"`
	Groups    []AccessMembership `json:"groups"`
	Bindings  []AccessBinding    `json:"bindings"`
	Tokens    []AccessToken      `json:"tokens"`
	Flags     []string           `json:"flags"`
}

// AccessMembership is a current group of a user, and when it lapses: the end of the lifetime of
// the latest login that brought it (docs/spec/rbac.md §8).
type AccessMembership struct {
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// AccessBinding is a binding as the bindings API answers it, with Via, the group:<name> of the
// group whose binding it is, for a user's binding through a group.
type AccessBinding struct {
	auth.Binding
	Via string `json:"via,omitempty"`
}

// AccessToken is a live token as the tokens API answers it, with its flags, and, for a deploy
// token, its principal token:<id>.
type AccessToken struct {
	auth.Token
	Principal string   `json:"principal,omitempty"`
	Flags     []string `json:"flags"`
}

// AccessGroup is a group that has a binding, with its bindings, its members (the live users whose
// current groups name it) and its flags.
type AccessGroup struct {
	Principal string          `json:"principal"`
	Bindings  []AccessBinding `json:"bindings"`
	Members   []AccessMember  `json:"members"`
	Flags     []string        `json:"flags"`
}

// AccessMember is a member of a group: the user's principal and name, and when the group lapses
// for it.
type AccessMember struct {
	Principal string    `json:"principal"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// AccessWorkload is a cert: or k8s: principal or pattern that has a binding (docs/spec/rbac.md
// §9), with its bindings and its flags.
type AccessWorkload struct {
	Principal string          `json:"principal"`
	Pattern   bool            `json:"pattern,omitempty"`
	Bindings  []AccessBinding `json:"bindings"`
	Flags     []string        `json:"flags"`
}

// AccessCIPolicy is a CI trust policy as GET /api/v1/ci/policies answers it, with its flags.
type AccessCIPolicy struct {
	ci.Policy
	Flags []string `json:"flags"`
}

// AccessAnonymous is what anonymous reads: the repos with anonymousRead, <format>/<name>.
type AccessAnonymous struct {
	Repos []string `json:"repos"`
}

// ReadAccessReport reads the rows of the report on r in one read (index.Reader.ReadAccess) and
// builds the document of q at the database's time of the read. A q out of its bounds (Check) is
// an error wrapping format.ErrInvalid, and nothing is read.
func ReadAccessReport(ctx context.Context, r index.Reader, c AccessConfig, q AccessQuery) (*AccessReport, error) {
	if err := q.Check(); err != nil {
		return nil, invalid("%s", err.Error())
	}
	a, err := r.ReadAccess(ctx)
	if err != nil {
		return nil, fmt.Errorf("api: read the access report: %w", err)
	}
	return NewAccessReport(a, c, q), nil
}

// Check returns why q is out of its bounds, its message starting with the field, scope or
// unusedDays; nil when it is within them.
func (q AccessQuery) Check() error {
	if err := auth.CheckAccessScope(q.Scope); err != nil {
		return errors.New(err.Error())
	}
	if q.UnusedDays < MinUnusedDays || q.UnusedDays > MaxUnusedDays {
		return fmt.Errorf("unusedDays: %d is not a number of days from %d to %d", q.UnusedDays, MinUnusedDays, MaxUnusedDays)
	}
	return nil
}

// NewAccessReport builds the document of q from the rows a, at a.ReadAt to the second, which is
// its generatedAt and the time its flags are worked out at: the same rows at the same time are
// the same document.
func NewAccessReport(a *index.Access, c AccessConfig, q AccessQuery) *AccessReport {
	b := accessBuilder{a: a, c: c, q: q, at: second(a.ReadAt)}
	b.cutoff = b.at.Add(-time.Duration(q.UnusedDays) * 24 * time.Hour)
	b.users = &auth.Users{InactiveAfter: c.InactiveAfter, Lockout: c.Lockout}
	b.tokens = &auth.Tokens{DeployMaxLifetime: c.DeployMaxTTL, ServiceMaxLifetime: c.ServiceMaxTTL}
	rep := &AccessReport{GeneratedAt: b.at, Scope: q.Scope, UnusedDays: q.UnusedDays, Settings: AccessSettings{
		AdminMFA: c.AdminMFA, AnonymousRead: c.AnonymousRead, DisableInactiveUsersAfter: seconds(c.InactiveAfter),
		DeployTokensMaxTTL: seconds(c.DeployMaxTTL), ServiceAccountsMaxTokenTTL: seconds(c.ServiceMaxTTL)}}
	b.fill()
	rep.Users = b.userSection()
	if q.Scope != auth.AccessSystems {
		rep.Groups = ptrTo(b.groupSection())
	}
	if q.Scope != auth.AccessPeople {
		rep.Workloads, rep.DeployTokens, rep.CIPolicies = ptrTo(b.workloadSection()), ptrTo(b.deploySection()), ptrTo(b.ciSection())
	}
	if c.AnonymousRead {
		rep.Anonymous = &AccessAnonymous{Repos: []string{}}
		for _, r := range a.AnonymousRepos {
			rep.Anonymous.Repos = append(rep.Anonymous.Repos, r.Format+"/"+r.Name)
		}
		slices.Sort(rep.Anonymous.Repos)
	}
	return rep
}

// accessBuilder builds the sections of a report from its rows.
type accessBuilder struct {
	a      *index.Access
	c      AccessConfig
	q      AccessQuery
	at     time.Time // generatedAt
	cutoff time.Time // at less unusedDays: a last use before it is unused
	users  *auth.Users
	tokens *auth.Tokens

	live     map[string]*index.User         // the live users by uid
	bindings map[string][]index.Binding     // the bindings by principal
	groups   map[int64]map[string]time.Time // the current groups of each user, by user id: when each lapses
	repos    map[int64]string               // the <format>/<name> of the repos of deploy tokens
	pats     map[int64][]index.Token        // the live personal access tokens by user id
	members  map[string][]AccessMember      // the members of each group, by group:<name>
}

// fill fills the maps of b from its rows.
func (b *accessBuilder) fill() {
	b.live, b.bindings, b.groups = map[string]*index.User{}, map[string][]index.Binding{}, map[int64]map[string]time.Time{}
	b.repos, b.pats, b.members = map[int64]string{}, map[int64][]index.Token{}, map[string][]AccessMember{}
	byID := map[int64]*index.User{}
	for i := range b.a.Users {
		u := &b.a.Users[i]
		b.live[u.UID], byID[u.ID] = u, u
	}
	for _, bd := range b.a.Bindings {
		b.bindings[bd.Principal] = append(b.bindings[bd.Principal], bd)
	}
	for _, g := range b.a.IdentityGroups {
		u := byID[g.UserID]
		if u == nil || g.ExpiresAt == nil || !g.ExpiresAt.After(b.a.ReadAt) {
			continue
		}
		if b.groups[u.ID] == nil {
			b.groups[u.ID] = map[string]time.Time{}
		}
		if until := second(*g.ExpiresAt); until.After(b.groups[u.ID][g.Name]) {
			b.groups[u.ID][g.Name] = until
		}
	}
	for id, groups := range b.groups {
		u := byID[id]
		for name, until := range groups {
			p := auth.Principal{Kind: auth.KindGroup, Name: name}.String()
			b.members[p] = append(b.members[p], AccessMember{Principal: auth.UserPrincipal(u).String(), Name: u.Name, ExpiresAt: until})
		}
	}
	for _, r := range b.a.TokenRepos {
		b.repos[r.ID] = r.Format + "/" + r.Name
	}
	for _, t := range b.a.Tokens {
		if t.Kind == auth.TokenPAT && t.UserID != nil {
			b.pats[*t.UserID] = append(b.pats[*t.UserID], t)
		}
	}
}

// userSection returns the users of the scope, by name.
func (b *accessBuilder) userSection() []AccessUser {
	out := []AccessUser{}
	for i := range b.a.Users {
		u := &b.a.Users[i]
		switch {
		case b.q.Scope == auth.AccessPeople && u.Kind == auth.KindService:
			continue
		case b.q.Scope == auth.AccessSystems && u.Kind != auth.KindService:
			continue
		}
		out = append(out, b.user(u))
	}
	slices.SortFunc(out, func(x, y AccessUser) int { return strings.Compare(x.Name, y.Name) })
	return out
}

// user returns the entry of the live user u.
func (b *accessBuilder) user(u *index.User) AccessUser {
	view := b.users.ViewAt(u, b.live, b.at)
	view.CreatedAt, view.LastSeenAt, view.LockedUntil = second(view.CreatedAt), secondPtr(view.LastSeenAt), secondPtr(view.LockedUntil)
	p := auth.UserPrincipal(u).String()
	e := AccessUser{User: view, Principal: p, Groups: []AccessMembership{}, Tokens: []AccessToken{}}
	e.Bindings = b.bindingsOf(p, "", u.Name)
	for name, until := range b.groups[u.ID] {
		e.Groups = append(e.Groups, AccessMembership{Name: name, ExpiresAt: until})
		g := auth.Principal{Kind: auth.KindGroup, Name: name}.String()
		e.Bindings = append(e.Bindings, b.bindingsOf(g, g, "")...)
	}
	slices.SortFunc(e.Groups, func(x, y AccessMembership) int { return strings.Compare(x.Name, y.Name) })
	sortBindings(e.Bindings)
	for _, t := range b.pats[u.ID] {
		e.Tokens = append(e.Tokens, b.token(&t, u, ""))
	}
	slices.SortFunc(e.Tokens, func(x, y AccessToken) int { return cmp.Compare(x.ID, y.ID) })
	f := bindingFlags(e.Bindings)
	f.adminWithoutEffect = b.c.AdminMFA && (u.Kind == auth.KindLocal || u.Kind == auth.KindService) &&
		slices.ContainsFunc(e.Bindings, func(bd AccessBinding) bool { return bd.Via == "" && bd.Role == auth.RoleAdmin })
	f.unused = auth.LastActivity(u).Before(b.cutoff)
	f.ownerDisabled, f.inactive = view.OwnerDisabled, view.Inactive
	e.Flags = f.list()
	return e
}

// bindingsOf returns the bindings of principal, each with via and with name, a user's name.
func (b *accessBuilder) bindingsOf(principal, via, name string) []AccessBinding {
	out := []AccessBinding{}
	for _, bd := range b.bindings[principal] {
		out = append(out, AccessBinding{Binding: auth.Binding{ID: bd.ID, Principal: bd.Principal, Name: name, Role: bd.Role,
			RepoMask: bd.RepoMask, CreatedAt: second(bd.CreatedAt)}, Via: via})
	}
	sortBindings(out)
	return out
}

// token returns the entry of the live token t, of the user u or of the repo repo.
func (b *accessBuilder) token(t *index.Token, u *index.User, repo string) AccessToken {
	view := b.tokens.ViewAt(t, u, repo, b.at)
	view.ExpiresAt, view.LastUsedAt, view.RevokedAt = secondPtr(view.ExpiresAt), secondPtr(view.LastUsedAt), secondPtr(view.RevokedAt)
	view.CreatedAt = second(view.CreatedAt)
	if view.SignIn != nil {
		view.SignIn.AuthTime = secondPtr(view.SignIn.AuthTime)
	}
	last := t.CreatedAt
	if t.LastUsedAt != nil {
		last = *t.LastUsedAt
	}
	f := flagSet{allRepos: t.RepoMask == auth.MaskAll, noExpiry: t.ExpiresAt == nil, unused: last.Before(b.cutoff),
		beyondMaxTTL: view.BeyondMaxTTL}
	return AccessToken{Token: view, Flags: f.list()}
}

// groupSection returns every group that has a binding, by principal.
func (b *accessBuilder) groupSection() []AccessGroup {
	out := []AccessGroup{}
	for p := range b.bindings {
		if pr, err := auth.ParsePrincipal(p); err != nil || pr.Kind != auth.KindGroup {
			continue
		}
		g := AccessGroup{Principal: p, Bindings: b.bindingsOf(p, "", ""), Members: slices.Clone(b.members[p])}
		if g.Members == nil {
			g.Members = []AccessMember{}
		}
		slices.SortFunc(g.Members, func(x, y AccessMember) int { return strings.Compare(x.Principal, y.Principal) })
		g.Flags = bindingFlags(g.Bindings).list()
		out = append(out, g)
	}
	slices.SortFunc(out, func(x, y AccessGroup) int { return strings.Compare(x.Principal, y.Principal) })
	return out
}

// workloadSection returns every cert: and k8s: principal or pattern that has a binding, by
// principal.
func (b *accessBuilder) workloadSection() []AccessWorkload {
	out := []AccessWorkload{}
	for p := range b.bindings {
		pr, err := auth.ParsePrincipal(p)
		if err != nil || pr.Kind != auth.KindCert && pr.Kind != auth.KindKubernetes {
			continue
		}
		w := AccessWorkload{Principal: p, Pattern: pr.IsPattern(), Bindings: b.bindingsOf(p, "", "")}
		w.Flags = bindingFlags(w.Bindings).list()
		out = append(out, w)
	}
	slices.SortFunc(out, func(x, y AccessWorkload) int { return strings.Compare(x.Principal, y.Principal) })
	return out
}

// deploySection returns every live deploy token, of a repo and over a mask, by id.
func (b *accessBuilder) deploySection() []AccessToken {
	out := []AccessToken{}
	for _, t := range b.a.Tokens {
		if t.Kind != auth.TokenDeploy {
			continue
		}
		repo := ""
		if t.RepoID != nil {
			repo = b.repos[*t.RepoID]
		}
		e := b.token(&t, nil, repo)
		e.Principal = auth.Principal{Kind: auth.KindToken, Name: strconv.FormatInt(t.ID, 10)}.String()
		out = append(out, e)
	}
	slices.SortFunc(out, func(x, y AccessToken) int { return cmp.Compare(x.ID, y.ID) })
	return out
}

// ciSection returns every CI trust policy, of the configuration and of the API, by name.
func (b *accessBuilder) ciSection() []AccessCIPolicy {
	out := []AccessCIPolicy{}
	for _, p := range ci.Views(b.c.CIPolicies, b.a.CIPolicies, b.at) {
		p.ExpiresAt, p.CreatedAt, p.UpdatedAt, p.ApprovedAt = secondPtr(p.ExpiresAt), secondPtr(p.CreatedAt), secondPtr(p.UpdatedAt),
			secondPtr(p.ApprovedAt)
		if p.Renewal != nil {
			p.Renewal = &ci.Renewal{ExpiresAt: second(p.Renewal.ExpiresAt)}
		}
		f := flagSet{noExpiry: p.ExpiresAt == nil,
			allRepos: slices.ContainsFunc(p.Grants, func(g index.CIGrant) bool { return g.Mask == auth.MaskAll })}
		out = append(out, AccessCIPolicy{Policy: p, Flags: f.list()})
	}
	slices.SortFunc(out, func(x, y AccessCIPolicy) int {
		return cmp.Or(strings.Compare(x.Name, y.Name), strings.Compare(x.Source, y.Source))
	})
	return out
}

// sortBindings sorts bindings by role, mask and via, then id.
func sortBindings(bindings []AccessBinding) {
	slices.SortFunc(bindings, func(x, y AccessBinding) int {
		return cmp.Or(strings.Compare(x.Role, y.Role), strings.Compare(x.RepoMask, y.RepoMask), strings.Compare(x.Via, y.Via),
			cmp.Compare(x.ID, y.ID))
	})
}

// flagSet is the flags that hold of an entry or a token.
type flagSet struct {
	admin, allRepos, adminWithoutEffect, noExpiry, unused, beyondMaxTTL, ownerDisabled, inactive bool
}

// bindingFlags are the flags that bindings give: admin for a binding of the role admin, allRepos
// for one on */*.
func bindingFlags(bindings []AccessBinding) flagSet {
	return flagSet{
		admin:    slices.ContainsFunc(bindings, func(b AccessBinding) bool { return b.Role == auth.RoleAdmin }),
		allRepos: slices.ContainsFunc(bindings, func(b AccessBinding) bool { return b.RepoMask == auth.MaskAll }),
	}
}

// list returns the flags of f that hold, in the order of the flags.
func (f flagSet) list() []string {
	out := []string{}
	for _, flag := range []struct {
		name string
		on   bool
	}{{FlagAdmin, f.admin}, {FlagAllRepos, f.allRepos}, {FlagAdminWithoutEffect, f.adminWithoutEffect}, {FlagNoExpiry, f.noExpiry},
		{FlagUnused, f.unused}, {FlagBeyondMaxTTL, f.beyondMaxTTL}, {FlagOwnerDisabled, f.ownerDisabled}, {FlagInactive, f.inactive}} {
		if flag.on {
			out = append(out, flag.name)
		}
	}
	return out
}

// JSON returns the document in its canonical form (docs/spec/rbac.md §14): compact JSON without
// insignificant whitespace, the keys of every object in byte order, and a line feed at the end.
func (rep *AccessReport) JSON() ([]byte, error) {
	raw, err := json.Marshal(rep)
	if err != nil {
		return nil, fmt.Errorf("api: encode the access report: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // numbers as they were written
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("api: encode the access report: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // a map's keys in byte order, and a line feed at the end
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("api: encode the access report: %w", err)
	}
	return buf.Bytes(), nil
}

// second is t in UTC to the second.
func second(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

// secondPtr is second of *t, nil for nil.
func secondPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	s := second(*t)
	return &s
}

// seconds is d in whole seconds.
func seconds(d time.Duration) int64 { return int64(d / time.Second) }

// ptrTo returns a pointer to v.
func ptrTo[T any](v T) *T { return &v }

// accessReport answers GET /api/v1/access-report (docs/spec/rbac.md §14): the admin role, checked
// before anything else, then the document of ?scope= and ?unusedDays= in its canonical form,
// read in one read and written as it is, so that its sha256 is that of the bytes the client
// gets. A read: it writes no audit row.
func (h *handler) accessReport(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	q, err := accessQueryOf(r)
	if err != nil {
		return 0, nil, err
	}
	rep, err := ReadAccessReport(r.Context(), h.repos.Index, h.access, q)
	if err != nil {
		return 0, nil, err
	}
	doc, err := rep.JSON()
	if err != nil {
		return 0, nil, err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(doc) //nolint:gosec // G705: JSON of the index, application/json with nosniff; a write error is a client gone
	return 0, nil, nil
}

// accessQueryOf returns the AccessQuery of ?scope= (all when absent) and ?unusedDays= (90 when
// absent); a value given twice, or out of its bounds, is 400 naming it.
func accessQueryOf(r *http.Request) (AccessQuery, error) {
	q := AccessQuery{Scope: auth.AccessAll, UnusedDays: DefaultUnusedDays}
	values := r.URL.Query()
	for _, key := range []string{"scope", "unusedDays"} {
		if len(values[key]) > 1 {
			return q, invalid("%s: once in the query", key)
		}
	}
	if v, ok := values["scope"]; ok {
		q.Scope = v[0]
	}
	if v, ok := values["unusedDays"]; ok {
		n, err := strconv.Atoi(v[0])
		if err != nil {
			return q, invalid("unusedDays: %q is not a number of days from %d to %d", v[0], MinUnusedDays, MaxUnusedDays)
		}
		q.UnusedDays = n
	}
	if err := q.Check(); err != nil {
		return q, invalid("%s", err.Error())
	}
	return q, nil
}
