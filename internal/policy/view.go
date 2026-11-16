package policy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// View is the policy of one repo for a render (docs/spec/policy.md §6.1): the rules, the groups
// of the repo and the verdicts that may hold in it, read once, and the decisions taken from
// them. A nil *View allows everything.
type View struct {
	ctx      context.Context
	r        index.Reader
	now      time.Time
	repo     index.Repo
	rules    []index.PolicyRule
	groups   []string
	verdicts map[digest.Digest][]index.Verdict
	order    VersionOrder

	mu    sync.Mutex
	until time.Time
}

// View reads, through r, what the decisions of a render of repo need: the rules, the groups of
// the repo, and its verdicts and those of any repo with a scope. A nil Decider returns a nil
// View.
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
	v := &View{ctx: ctx, r: r, now: d.o.Now(), repo: repo, rules: rules, groups: groups, order: d.order(repo.Format),
		verdicts: map[digest.Digest][]index.Verdict{}}
	for _, row := range rows {
		v.verdicts[row.Subject] = append(v.verdicts[row.Subject], row)
	}
	return v, nil
}

// Decide decides s, whose Repo is the view's, and keeps the earliest Until.
func (v *View) Decide(s Subject) Verdict {
	if v == nil {
		return Verdict{Effect: EffectAllow}
	}
	e := evaluation{now: v.now, subject: s, rules: v.rules, groups: v.groups, verdicts: v.verdicts[s.Digest], order: v.order}
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

// Until is the earliest Until of the decisions taken so far: the expiry of what was rendered
// from them (index_cache.expires_at); nil for none.
func (v *View) Until() *time.Time {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.until.IsZero() {
		return nil
	}
	t := v.until
	return &t
}

// Reader returns r without the versions of the view's repo that the view refuses: what a
// renderer reads (docs/spec/policy.md §6.1). A nil View returns r.
func (v *View) Reader(r index.Reader) index.Reader {
	if v == nil {
		return r
	}
	return &reader{Reader: r, v: v, names: map[int64]string{}}
}

// reader is an index.Reader that leaves out the versions its view refuses, in every method that
// returns a version of the view's repo. It learns a package's name from the packages it returns,
// and from the repo's whole list when it meets a package it does not know.
type reader struct {
	index.Reader
	v      *View
	names  map[int64]string
	listed bool
}

// learn records the names of pkgs.
func (r *reader) learn(pkgs ...index.Package) {
	for _, p := range pkgs {
		if p.RepoID == r.v.repo.ID {
			r.names[p.ID] = p.Name
		}
	}
}

// nameOf returns the name of the package id of the view's repo; false for another repo's.
func (r *reader) nameOf(ctx context.Context, id int64) (string, bool, error) {
	if n, ok := r.names[id]; ok {
		return n, true, nil
	}
	if !r.listed {
		r.listed = true
		pkgs, err := r.Reader.ListPackages(ctx, r.v.repo.ID, "")
		if err != nil {
			return "", false, err
		}
		r.learn(pkgs...)
	}
	n, ok := r.names[id]
	return n, ok, nil
}

// allowed reports whether the view lets the version v of the package name through.
func (r *reader) allowed(name string, v index.Version) bool {
	return r.v.Decide(Subject{Repo: r.v.repo, Name: name, Version: v.Version, Variant: v.Variant, Digest: v.Digest,
		Published: v.CreatedAt}).Allowed()
}

// keep returns the versions of vs that the view lets through.
func (r *reader) keep(ctx context.Context, vs []index.Version) ([]index.Version, error) {
	out := vs[:0:0]
	for _, v := range vs {
		name, ok, err := r.nameOf(ctx, v.PackageID)
		if err != nil {
			return nil, err
		}
		if !ok || r.allowed(name, v) {
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

func (r *reader) ListVersionPage(ctx context.Context, packageID int64, after *index.VersionKey, limit int, without ...string) ([]index.Version, error) {
	vs, err := r.Reader.ListVersionPage(ctx, packageID, after, limit, without...)
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
	if err != nil || repoID != r.v.repo.ID {
		return ms, err
	}
	out := ms[:0:0]
	for _, m := range ms {
		if r.allowed(m.Package.Name, m.Version) {
			out = append(out, m)
		}
	}
	return out, nil
}
