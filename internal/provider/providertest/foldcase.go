package providertest

import (
	"context"
	"io"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// FoldCase returns f as a registry that folds the case of tags, as Gitea and Forgejo do
// (docs/registries/gitea.md, Tags): a tag is stored lower-cased, so the tag list names
// 5.4.final for a push to 5.4.Final, a push to 5.4.final re-points it, and a tag resolves in
// any case. A digest is lower-case already.
func FoldCase(f *Fake) provider.Provider { return foldCase{f} }

type foldCase struct{ *Fake }

func (c foldCase) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := c.Fake.Repository(ctx, name)
	return foldCaseRepo{rr}, err
}

type foldCaseRepo struct{ provider.Repository }

func (r foldCaseRepo) PushReference(ctx context.Context, expected ocispec.Descriptor, content io.Reader, reference string) error {
	return r.Repository.PushReference(ctx, expected, content, strings.ToLower(reference))
}

func (r foldCaseRepo) Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error {
	return r.Repository.Tag(ctx, desc, strings.ToLower(reference))
}

func (r foldCaseRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	return r.Repository.Resolve(ctx, strings.ToLower(reference))
}
