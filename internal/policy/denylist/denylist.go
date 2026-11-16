// Package denylist builds the deny list of docs/spec/policy.md §8: the rules and the verdicts in
// force that refuse, the digests and the URL paths they refuse, as a document for the flat JSON
// of the management API and, signed with the policy key, as an OCI artifact in the registry
// (docs/spec/manifest-profile.md §10).
package denylist

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The artifact of the deny list (docs/spec/manifest-profile.md §10).
const (
	Repository        = "lajstrom/policy/denylist"
	ArtifactType      = "application/vnd.lajstrom.policy.denylist.v1"
	DocumentMediaType = "application/vnd.lajstrom.policy.denylist.v1+json"
	SignatureType     = "application/vnd.lajstrom.policy.denylist.signature.v1"
	DocumentTitle     = "denylist.json"
	SignatureTitle    = "denylist.json.sig"
	// Version is the version of the document.
	Version = 1
)

// Document is the deny list (docs/spec/policy.md §8).
type Document struct {
	Version    int             `json:"version"`
	Generation int64           `json:"generation"`
	CreatedAt  time.Time       `json:"createdAt"`
	Rules      []Rule          `json:"rules"`
	Verdicts   []Verdict       `json:"verdicts"`
	Digests    []digest.Digest `json:"digests"`
	Prefixes   []string        `json:"prefixes"`
}

// Rule is a deny rule in force.
type Rule struct {
	ID        int64      `json:"id"`
	RepoMask  string     `json:"repoMask"`
	Name      string     `json:"name"`
	Version   string     `json:"version"`
	Variant   string     `json:"variant"`
	Reason    string     `json:"reason"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// Verdict is a verdict in force that refuses.
type Verdict struct {
	Repo          string        `json:"repo"`
	OCIRepository string        `json:"ociRepository"`
	Subject       digest.Digest `json:"subject"`
	Verdict       string        `json:"verdict"`
	Reason        string        `json:"reason"`
	Source        string        `json:"source"`
	Scope         string        `json:"scope"`
	ValidUntil    *time.Time    `json:"validUntil"`
	Attestation   digest.Digest `json:"attestation"`
}

// Locate is the format.Locator of a repo's format: the path that serves the files of a version.
type Locate func(repo index.Repo, name, version string) (string, bool)

// Build reads the deny list through r at the database's time. keep chooses the repos whose
// entries it holds, nil every repo: a rule is in it when its mask matches a repo kept, a verdict
// when its repo is kept. locate gives a version's own download path; nil gives none.
func Build(ctx context.Context, r index.Reader, keep func(index.Repo) bool, locate Locate) (*Document, error) {
	gen, err := r.PolicyGeneration(ctx)
	if err != nil {
		return nil, err
	}
	now, err := r.DatabaseTime(ctx)
	if err != nil {
		return nil, err
	}
	all, err := r.ListRepos(ctx, "")
	if err != nil {
		return nil, err
	}
	b := &builder{ctx: ctx, r: r, now: now.UTC(), locate: locate, repos: map[int64]index.Repo{}, digests: map[digest.Digest]bool{},
		prefixes: map[string]bool{}, doc: &Document{Version: Version, Generation: gen, CreatedAt: now.UTC(), Rules: []Rule{},
			Verdicts: []Verdict{}}}
	for _, repo := range all {
		if repo.Kind != index.KindGroup && (keep == nil || keep(repo)) {
			b.repos[repo.ID] = repo
			b.kept = append(b.kept, repo)
		}
	}
	if err := b.rules(); err != nil {
		return nil, err
	}
	if err := b.verdicts(); err != nil {
		return nil, err
	}
	for d := range b.digests {
		b.doc.Digests = append(b.doc.Digests, d)
	}
	for p := range b.prefixes {
		b.doc.Prefixes = append(b.doc.Prefixes, p)
	}
	slices.Sort(b.doc.Digests)
	slices.Sort(b.doc.Prefixes)
	return b.doc, nil
}

// builder is one Build.
type builder struct {
	ctx      context.Context
	r        index.Reader
	now      time.Time
	locate   Locate
	repos    map[int64]index.Repo // the repos kept, by id
	kept     []index.Repo
	digests  map[digest.Digest]bool
	prefixes map[string]bool
	doc      *Document
}

// rules adds the deny rules in force whose mask matches a repo kept, and what a rule of one exact
// name and version refuses in each such repo.
func (b *builder) rules() error {
	rows, err := b.r.ListPolicyRules(b.ctx)
	if err != nil {
		return err
	}
	for _, rule := range rows {
		if rule.Effect != index.PolicyDeny || rule.Expired(b.now) {
			continue
		}
		var matched []index.Repo
		for _, repo := range b.kept {
			if auth.MaskMatches(rule.RepoMask, repo.Format, repo.Name) {
				matched = append(matched, repo)
			}
		}
		if len(matched) == 0 {
			continue
		}
		b.doc.Rules = append(b.doc.Rules, Rule{ID: rule.ID, RepoMask: rule.RepoMask, Name: rule.Name, Version: rule.Version,
			Variant: rule.Variant, Reason: rule.Reason, ExpiresAt: rule.ExpiresAt})
		version, exact := exactVersion(rule.Version)
		if !exact || strings.Contains(rule.Name, "*") {
			continue
		}
		for _, repo := range matched {
			if err := b.exactRule(repo, rule, version); err != nil {
				return err
			}
		}
	}
	return nil
}

// exactVersion returns the one version a constraint names, ==<version> or <version> without a
// glob; false for any other.
func exactVersion(constraint string) (string, bool) {
	c := strings.TrimSpace(constraint)
	c = strings.TrimSpace(strings.TrimPrefix(c, "=="))
	if c == "" || c == "*" || strings.ContainsAny(c, "*,<>!= ") {
		return "", false
	}
	return c, true
}

// exactRule adds what the rule of the one name and version refuses in repo: its own download path,
// held or not, and the digests and paths of the variants the repo holds.
func (b *builder) exactRule(repo index.Repo, rule index.PolicyRule, version string) error {
	b.locatePath(repo, rule.Name, version)
	pkg, err := b.r.GetPackage(b.ctx, repo.ID, rule.Name)
	if errors.Is(err, index.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	vs, err := b.r.ListVersions(b.ctx, pkg.ID)
	if err != nil {
		return err
	}
	for _, v := range vs {
		if v.Version == version && (rule.Variant == "*" || strings.EqualFold(rule.Variant, v.Variant)) {
			if err := b.version(repo, *pkg, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// verdicts adds the verdicts in force that refuse, of the repos kept, and what they refuse.
func (b *builder) verdicts() error {
	rows, err := b.r.ListVerdicts(b.ctx, index.VerdictFilter{})
	if err != nil {
		return err
	}
	for _, v := range rows {
		repo, ok := b.repos[v.RepoID]
		if !ok || v.Expired(b.now) || (v.Verdict != index.PolicyDeny && v.Verdict != index.PolicyQuarantine) {
			continue
		}
		oci := base(repo) + "/" + v.Path
		b.doc.Verdicts = append(b.doc.Verdicts, Verdict{Repo: repo.Format + "/" + repo.Name, OCIRepository: oci, Subject: v.Subject,
			Verdict: v.Verdict, Reason: v.Reason, Source: v.Source, Scope: v.Scope, ValidUntil: v.ValidUntil, Attestation: v.Referrer})
		b.digests[v.Subject] = true
		b.prefixes["/v2/"+oci+"/manifests/"+v.Subject.String()] = true
		if v.AttachmentID == 0 {
			continue
		}
		pv, err := b.r.GetVersionByDigest(b.ctx, repo.ID, v.Subject)
		if errors.Is(err, index.ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		pkg, err := index.PackageOf(b.ctx, b.r, repo.ID, v.Path)
		if errors.Is(err, index.ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		if err := b.version(repo, *pkg, *pv); err != nil {
			return err
		}
	}
	return nil
}

// version adds the manifest and the files of the version v, its /v2 path and its own download
// path.
func (b *builder) version(repo index.Repo, pkg index.Package, v index.Version) error {
	b.digests[v.Digest] = true
	b.prefixes["/v2/"+base(repo)+"/"+index.ManifestPath(pkg, v)+"/manifests/"+v.Digest.String()] = true
	b.locatePath(repo, pkg.Name, v.Version)
	files, err := b.r.ListFiles(b.ctx, v.ID)
	if err != nil {
		return fmt.Errorf("denylist: the files of %s %s: %w", pkg.Name, v.Version, err)
	}
	for _, f := range files {
		b.digests[f.Digest] = true
	}
	return nil
}

// locatePath adds the download path of the version of name in repo, when its format has one.
func (b *builder) locatePath(repo index.Repo, name, version string) {
	if b.locate == nil {
		return
	}
	if p, ok := b.locate(repo, name, version); ok {
		b.prefixes[p] = true
	}
}

// base is the path of repo's OCI repositories below the instance prefix.
func base(repo index.Repo) string {
	if b := strings.Trim(repo.Prefix, "/"); b != "" {
		return b
	}
	return repo.Format + "/" + repo.Name
}
