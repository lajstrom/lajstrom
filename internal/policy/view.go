package policy

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// View is the policy of one repo for a render (docs/spec/policy.md §6.1): the rules, the groups
// of the repo, the verdicts that may hold in it and, on a repo with requirements (§11), the
// verified attachments of its versions, read once, and the decisions taken from them. The view of
// a group decides a version of a member as the member's own view does, read when the render first
// meets one of its versions: a decision is taken in the repo that holds the artifact (§1.1), so
// the members' requirements hold through the group. A nil *View allows everything.
type View struct {
	ctx      context.Context
	r        index.Reader
	d        *Decider
	now      time.Time
	repo     index.Repo
	rules    []index.PolicyRule
	groups   []string
	verdicts map[digest.Digest][]index.Verdict
	order    VersionOrder
	reqs     requirements
	attested map[int64][]index.Attested // by version id, on a repo with requirements
	members  map[int64]index.Repo       // of a group, by id

	mu    sync.Mutex
	until time.Time
	views map[int64]*View // the members' views read so far, by id
}

// View reads, through r, what the decisions of a render of repo need: the rules, the groups of
// the repo, its verdicts and those of any repo with a scope, the verified attachments of its
// versions when it has requirements (ListAttested), and the members of a group. A nil Decider
// returns a nil View.
func (d *Decider) View(ctx context.Context, r index.Reader, repo index.Repo) (*View, error) {
	if d == nil {
		return nil, nil
	}
	rules, err := d.Rules(ctx, r)
	if err != nil {
		return nil, err
	}
	groups, err := groupsFor(ctx, r, repo, rules)
	if err != nil {
		return nil, err
	}
	rows, err := r.ListVerdicts(ctx, index.VerdictFilter{RepoID: repo.ID, Scoped: true})
	if err != nil {
		return nil, fmt.Errorf("policy: the verdicts of %s/%s: %w", repo.Format, repo.Name, err)
	}
	v := &View{ctx: ctx, r: r, d: d, now: d.o.Now(), repo: repo, rules: rules, groups: groups, order: d.order(repo.Format),
		verdicts: map[digest.Digest][]index.Verdict{}, reqs: requirementsOf(repo, d.o.Trust)}
	for _, row := range rows {
		v.verdicts[row.Subject] = append(v.verdicts[row.Subject], row)
	}
	if len(v.reqs.list) > 0 {
		attested, err := r.ListAttested(ctx, repo.ID, nil)
		if err != nil {
			return nil, fmt.Errorf("policy: the verified attachments of %s/%s: %w", repo.Format, repo.Name, err)
		}
		v.attested = map[int64][]index.Attested{}
		for _, a := range attested {
			v.attested[a.VersionID] = append(v.attested[a.VersionID], a)
		}
	}
	if repo.Kind == index.KindGroup {
		members, err := r.ListGroupMembers(ctx, repo.ID)
		if err != nil {
			return nil, fmt.Errorf("policy: the members of %s/%s: %w", repo.Format, repo.Name, err)
		}
		v.members = map[int64]index.Repo{}
		for _, m := range members {
			v.members[m.ID] = m
		}
	}
	return v, nil
}

// Decide decides s, whose Repo is the view's, and keeps the earliest Until. On a repo with
// requirements the version of s's digest is read for its attachments; a digest that is no
// version's, or a read that fails, has none.
func (v *View) Decide(s Subject) Verdict {
	if v == nil {
		return Verdict{Effect: EffectAllow}
	}
	var attested []index.Attested
	if len(v.reqs.list) > 0 && !s.NoRequirements && s.Digest != "" {
		if ver, err := v.r.GetVersionByDigest(v.ctx, v.repo.ID, s.Digest); err == nil {
			attested = v.attested[ver.ID]
		}
	}
	return v.decide(s, attested)
}

// decideVersion decides the version ver of the package name of the view's repo.
func (v *View) decideVersion(name string, ver index.Version) Verdict {
	return v.decide(Subject{Repo: v.repo, Name: name, Version: ver.Version, Variant: ver.Variant, Digest: ver.Digest,
		Published: ver.CreatedAt}, v.attested[ver.ID])
}

// decide decides s, whose manifest has the verified attachments attested, and keeps the earliest
// Until.
func (v *View) decide(s Subject, attested []index.Attested) Verdict {
	e := evaluation{now: v.now, subject: s, rules: v.rules, groups: v.groups, verdicts: v.verdicts[s.Digest], order: v.order,
		attested: attested}
	if !s.NoRequirements {
		e.reqs = v.reqs
	}
	e.seen = func() (time.Time, bool) { return seen(v.ctx, v.r, s) }
	d := e.decide()
	if !d.Until.IsZero() {
		v.mu.Lock()
		if v.until.IsZero() || d.Until.Before(v.until) {
			v.until = d.Until
		}
		v.mu.Unlock()
	}
	return d
}

// holds reports whether the view decides the versions of the repo id: its own, or a member's of
// its group.
func (v *View) holds(id int64) bool {
	_, member := v.members[id]
	return id == v.repo.ID || member
}

// viewOf returns the view that decides the versions of the repo id, which holds reports: the
// view itself, or the member's view, read the first time.
func (v *View) viewOf(id int64) (*View, error) {
	m, ok := v.members[id]
	if id == v.repo.ID || !ok {
		return v, nil
	}
	v.mu.Lock()
	mv := v.views[id]
	v.mu.Unlock()
	if mv != nil {
		return mv, nil
	}
	mv, err := v.d.View(v.ctx, v.r, m)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.views == nil {
		v.views = map[int64]*View{}
	}
	if have := v.views[id]; have != nil {
		return have, nil
	}
	v.views[id] = mv
	return mv, nil
}

// Until is the earliest Until of the decisions taken so far, a group's members' views included:
// the expiry of what was rendered from them (index_cache.expires_at); nil for none.
func (v *View) Until() *time.Time {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	until, views := v.until, slices.Collect(maps.Values(v.views))
	v.mu.Unlock()
	for _, mv := range views {
		if t := mv.Until(); t != nil && (until.IsZero() || t.Before(until)) {
			until = *t
		}
	}
	if until.IsZero() {
		return nil
	}
	return &until
}

// Reader returns r without the versions that the view refuses, of its repo or of a member of its
// group: what a renderer reads (docs/spec/policy.md §6.1). A nil View returns r.
func (v *View) Reader(r index.Reader) index.Reader {
	if v == nil {
		return r
	}
	return &reader{Reader: r, v: v, pkgs: map[int64]pkgRef{}}
}

// pkgRef is what a reader knows of a package: its name and its repo.
type pkgRef struct {
	name string
	repo int64
}

// reader is an index.Reader that leaves out the versions its view refuses, in every method that
// returns a version of a repo the view holds. It learns a package's name from the packages it
// returns, and from the whole lists of the repos the view holds when it meets a package it does
// not know.
type reader struct {
	index.Reader
	v      *View
	pkgs   map[int64]pkgRef
	listed bool
}

// learn records the names of pkgs.
func (r *reader) learn(pkgs ...index.Package) {
	for _, p := range pkgs {
		if r.v.holds(p.RepoID) {
			r.pkgs[p.ID] = pkgRef{name: p.Name, repo: p.RepoID}
		}
	}
}

// packageOf returns the package id of a repo the view holds; false for another repo's.
func (r *reader) packageOf(ctx context.Context, id int64) (pkgRef, bool, error) {
	if p, ok := r.pkgs[id]; ok {
		return p, true, nil
	}
	if !r.listed {
		r.listed = true
		repos := append([]int64{r.v.repo.ID}, slices.Sorted(maps.Keys(r.v.members))...)
		for _, repo := range repos {
			pkgs, err := r.Reader.ListPackages(ctx, repo, "")
			if err != nil {
				return pkgRef{}, false, err
			}
			r.learn(pkgs...)
		}
	}
	p, ok := r.pkgs[id]
	return p, ok, nil
}

// allowed reports whether the view lets the version v of the package p through.
func (r *reader) allowed(p pkgRef, v index.Version) (bool, error) {
	view, err := r.v.viewOf(p.repo)
	if err != nil {
		return false, err
	}
	return view.decideVersion(p.name, v).Allowed(), nil
}

// keep returns the versions of vs that the view lets through.
func (r *reader) keep(ctx context.Context, vs []index.Version) ([]index.Version, error) {
	out := vs[:0:0]
	for _, v := range vs {
		p, ok, err := r.packageOf(ctx, v.PackageID)
		if err != nil {
			return nil, err
		}
		allowed := !ok
		if ok {
			if allowed, err = r.allowed(p, v); err != nil {
				return nil, err
			}
		}
		if allowed {
			out = append(out, v)
		}
	}
	return out, nil
}

// one returns v, or ErrNotFound when the view refuses it.
func (r *reader) one(ctx context.Context, v *index.Version, err error) (*index.Version, error) {
	if err != nil {
		return nil, err
	}
	kept, err := r.keep(ctx, []index.Version{*v})
	switch {
	case err != nil:
		return nil, err
	case len(kept) == 0:
		return nil, fmt.Errorf("%w: version %s %q is refused by policy", index.ErrNotFound, v.Version, v.Variant)
	}
	return v, nil
}

func (r *reader) GetPackage(ctx context.Context, repoID int64, name string) (*index.Package, error) {
	p, err := r.Reader.GetPackage(ctx, repoID, name)
	if err == nil {
		r.learn(*p)
	}
	return p, err
}

func (r *reader) GetPackageByNameOCI(ctx context.Context, repoID int64, nameOCI string) (*index.Package, error) {
	p, err := r.Reader.GetPackageByNameOCI(ctx, repoID, nameOCI)
	if err == nil {
		r.learn(*p)
	}
	return p, err
}

func (r *reader) ListPackages(ctx context.Context, repoID int64, prefix string) ([]index.Package, error) {
	pkgs, err := r.Reader.ListPackages(ctx, repoID, prefix)
	if err == nil {
		r.learn(pkgs...)
	}
	return pkgs, err
}

func (r *reader) ListPackagesAfter(ctx context.Context, repoID int64, prefix, after string, limit int) ([]index.Package, error) {
	pkgs, err := r.Reader.ListPackagesAfter(ctx, repoID, prefix, after, limit)
	if err == nil {
		r.learn(pkgs...)
	}
	return pkgs, err
}

func (r *reader) ListVersions(ctx context.Context, packageID int64) ([]index.Version, error) {
	vs, err := r.Reader.ListVersions(ctx, packageID)
	if err != nil {
		return nil, err
	}
	return r.keep(ctx, vs)
}

func (r *reader) ListVersionPage(ctx context.Context, packageID int64, after *index.VersionKey, limit int, f index.VersionFilter) ([]index.Version, error) {
	vs, err := r.Reader.ListVersionPage(ctx, packageID, after, limit, f)
	if err != nil {
		return nil, err
	}
	return r.keep(ctx, vs)
}

func (r *reader) GetVersion(ctx context.Context, packageID int64, version, variant string) (*index.Version, error) {
	v, err := r.Reader.GetVersion(ctx, packageID, version, variant)
	return r.one(ctx, v, err)
}

func (r *reader) GetVersionByTag(ctx context.Context, packageID int64, tag string) (*index.Version, error) {
	v, err := r.Reader.GetVersionByTag(ctx, packageID, tag)
	return r.one(ctx, v, err)
}

func (r *reader) GetVersionByDigest(ctx context.Context, repoID int64, d digest.Digest) (*index.Version, error) {
	v, err := r.Reader.GetVersionByDigest(ctx, repoID, d)
	return r.one(ctx, v, err)
}

func (r *reader) FindFiles(ctx context.Context, repoID int64, name string) ([]index.FileMatch, error) {
	ms, err := r.Reader.FindFiles(ctx, repoID, name)
	if err != nil || !r.v.holds(repoID) {
		return ms, err
	}
	out := ms[:0:0]
	for _, m := range ms {
		allowed, err := r.allowed(pkgRef{name: m.Package.Name, repo: repoID}, m.Version)
		if err != nil {
			return nil, err
		}
		if allowed {
			out = append(out, m)
		}
	}
	return out, nil
}
