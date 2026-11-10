package ecr

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/lajstrom/lajstrom/internal/provider"
)

const (
	// knownFor is how long EnsureRepository believes, without asking ECR, that a repository it
	// created or found exists: ingest ensures the repository of every publish. A repository
	// deleted behind Lajstrom's back is created again after at most this long.
	knownFor = 10 * time.Minute
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
		return translate("ensure repository "+name, err)
	}
	p.known.Store(name, p.now().Add(knownFor))
	return nil
}

// ListRepositories is DescribeRepositories, in the next part of #36; until then it is
// unsupported, and callers use the oci_repos table.
func (p *Provider) ListRepositories(_ context.Context, prefix string) ([]string, error) {
	return nil, fmt.Errorf("ecr: list repositories %s: %w", prefix, provider.ErrUnsupported)
}

// DeleteRepository is DeleteRepository of the ECR API, in the next part of #36; until then
// it is unsupported.
func (p *Provider) DeleteRepository(_ context.Context, name string) error {
	return fmt.Errorf("ecr: delete repository %s: %w", name, provider.ErrUnsupported)
}
