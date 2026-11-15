package deb

import (
	"context"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// apt has group repos of hosted apt repos (docs/spec/proxy-and-group.md §8.6, http-routing.md,
// apt, Group): a group renders the documents of its suites from its members' versions, and
// format.CachedDocument keeps them under the group, which signs them with its own keys.
var _ format.IndexGrouper = Plugin{}

// RenderGroupIndex renders every document of one suite of the group, the scope, as RenderIndex
// renders a hosted repo's, from the versions of members, in order: of the versions of one
// package, version and architecture (ArchSource for a source package) in one component of the
// suite, the first member's; and a version of a member is left out when an earlier member holds
// one of its pool files under the same name with other bytes (clashes), so that the group holds
// one content per pool file name, as a hosted repo does, and its pool route, which serves the
// file of the first member that has it, serves the file its indexes list. The Release is the
// group's: its Label is the group's name. A suite that no member has is index.ErrNotFound.
func (p Plugin) RenderGroupIndex(ctx context.Context, group index.Repo, members []index.Repo, scope format.IndexScope, st index.Reader) ([]format.Document, error) {
	suite := string(scope)
	if checkDistName("suite", suite) != nil {
		return nil, fmt.Errorf("%w: apt/%s: %q is not a suite", index.ErrNotFound, group.Name, suite)
	}
	var (
		paras, srcs []paragraph
		earlier     []member
	)
	taken := map[[4]string]bool{} // component, name, version and architecture of a paragraph merged
	for _, m := range members {
		pkgs, err := st.ListPackages(ctx, m.ID, "")
		if err != nil {
			return nil, fmt.Errorf("deb: packages of apt/%s: %w", m.Name, err)
		}
		mp, ms, err := p.paragraphs(ctx, m, pkgs, suite, st)
		if err != nil {
			return nil, err
		}
		for _, para := range append(mp, ms...) {
			key := [4]string{para.component, para.name, para.version, para.arch}
			if taken[key] {
				continue
			}
			switch clash, err := clashes(ctx, st, earlier, para); {
			case err != nil:
				return nil, err
			case clash:
				continue
			}
			taken[key] = true
			if para.arch == ArchSource {
				srcs = append(srcs, para)
			} else {
				paras = append(paras, para)
			}
		}
		names := make(map[string]bool, len(pkgs))
		for _, pkg := range pkgs {
			names[pkg.Name] = true
		}
		earlier = append(earlier, member{repo: m, names: names})
	}
	if len(paras) == 0 && len(srcs) == 0 {
		return nil, fmt.Errorf("%w: no member of apt/%s has a package in the suite %s", index.ErrNotFound, group.Name, suite)
	}
	sortParagraphs(paras)
	sortParagraphs(srcs)
	return p.renderSuite(group, suite, paras, srcs)
}

// member is a member of a group that RenderGroupIndex has merged, and the names of its packages.
type member struct {
	repo  index.Repo
	names map[string]bool
}

// clashes reports whether one of the pool files of para is held by a member of earlier under
// its name with other bytes: a file whose layer digest is not sha256:<the paragraph's sha256>.
// A pool file is named after its package (<package>_<version>_<arch>.deb, <source>_<version>.dsc
// and the files the .dsc names), so only a member that has a package of the paragraph's name is
// asked.
func clashes(ctx context.Context, st index.Reader, earlier []member, para paragraph) (bool, error) {
	for _, e := range earlier {
		if !e.names[para.name] {
			continue
		}
		for name, sum := range para.files {
			held, err := st.FindFiles(ctx, e.repo.ID, name)
			if err != nil {
				return false, fmt.Errorf("deb: the files called %s in apt/%s: %w", name, e.repo.Name, err)
			}
			for _, f := range held {
				if f.File.Digest.Encoded() != sum {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
