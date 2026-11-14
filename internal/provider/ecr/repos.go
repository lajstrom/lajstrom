package ecr

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"

	"github.com/lajstrom/lajstrom/internal/provider"
)

const (
	// knownFor is how long EnsureRepository believes, without asking ECR, that a repository it
	// created or found exists: ingest ensures the repository of every publish. A repository
	// deleted behind Lajstrom's back is created again after at most this long.
	knownFor = 10 * time.Minute
	// pageSize is the page size of DescribeRepositories, its maximum.
	pageSize = 1000
)

// EnsureRepository creates the repository name below the instance prefix with CreateRepository,
// with the tag mutability, exclusion filters and encryption of registries[].ecr; a repository
// that exists is left as it is, its own settings included. A repository is asked for once per
// knownFor. It asks nothing with capabilities.createOnPush.
func (p *Provider) EnsureRepository(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("ecr: ensure repository: empty name")
	}
	if p.createOnPush {
		return nil
	}
	if until, ok := p.known.Load(name); ok && p.now().Before(until.(time.Time)) {
		return nil
	}
	in := p.create
	in.RepositoryName = aws.String(p.prefix + name)
	in.RegistryId = p.registryID
	_, err := p.api.CreateRepository(ctx, &in)
	switch {
	case err == nil:
		p.log.InfoContext(ctx, "ECR repository created", "repository", name,
			"imageTagMutability", string(in.ImageTagMutability), "encryption", string(in.EncryptionConfiguration.EncryptionType))
	case !isCode(err, "RepositoryAlreadyExistsException"):
		err = translate("ensure repository "+name, err)
		if errors.Is(err, provider.ErrLimit) {
			p.log.WarnContext(ctx, "ECR refused to create a repository for a service quota of the account; raise it or free room",
				"repository", name, "error", err.Error())
		}
		return err
	}
	p.known.Store(name, p.now().Add(knownFor))
	return nil
}

// ListRepositories lists the registry's repositories with DescribeRepositories, every page,
// and returns the names below the instance prefix that start with prefix, without the
// instance prefix, sorted. ECR filters by exact names only, so the prefix is applied here. It
// returns an error wrapping provider.ErrUnsupported when capabilities.catalog is false.
func (p *Provider) ListRepositories(ctx context.Context, prefix string) ([]string, error) {
	op := "list repositories " + prefix
	if !p.catalog {
		return nil, fmt.Errorf("ecr: %s: capabilities.catalog is false: %w", op, provider.ErrUnsupported)
	}
	want := p.prefix + prefix
	pages := ecr.NewDescribeRepositoriesPaginator(p.api,
		&ecr.DescribeRepositoriesInput{RegistryId: p.registryID, MaxResults: aws.Int32(pageSize)},
		func(o *ecr.DescribeRepositoriesPaginatorOptions) { o.StopOnDuplicateToken = true })
	var names []string
	for pages.HasMorePages() {
		out, err := pages.NextPage(ctx)
		if err != nil {
			return nil, translate(op, err)
		}
		for _, r := range out.Repositories {
			if n := aws.ToString(r.RepositoryName); strings.HasPrefix(n, want) {
				names = append(names, strings.TrimPrefix(n, p.prefix))
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// DeleteRepository deletes the repository name below the instance prefix with
// DeleteRepository. With registries[].ecr.forceDelete it deletes the images too; without, ECR
// refuses a repository that holds any, which is provider.ErrConflict. A repository that does
// not exist is provider.ErrNotFound.
func (p *Provider) DeleteRepository(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("ecr: delete repository: empty name")
	}
	p.known.Delete(name)
	_, err := p.api.DeleteRepository(ctx, &ecr.DeleteRepositoryInput{
		RepositoryName: aws.String(p.prefix + name),
		RegistryId:     p.registryID,
		Force:          p.forceDelete,
	})
	if err != nil {
		return translate("delete repository "+name, err)
	}
	p.log.InfoContext(ctx, "ECR repository deleted", "repository", name, "force", p.forceDelete)
	return nil
}
