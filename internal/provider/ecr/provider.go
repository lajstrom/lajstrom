package ecr

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// Type is the registries[].type of this provider.
const Type = "ecr"

// maxLayerSize is ECR's quota on the size of a layer, 52,000 MiB, from the AWS service quotas
// page: to be measured by #19.
const maxLayerSize = 52000 << 20

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.Pinger   = (*Provider)(nil)
)

// api is the part of the ECR API the provider uses: *ecr.Client has it, the tests fake it.
type api interface {
	tokenAPI
	CreateRepository(ctx context.Context, in *ecr.CreateRepositoryInput, optFns ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error)
}

// Provider is the Provider of one Amazon ECR registry. The data path, /v2, is the generic
// provider's, with the authorization token as its Basic credentials; the repositories are
// created, listed and deleted with the ECR API. It is safe for concurrent use.
type Provider struct {
	name         string
	prefix       string  // the instance prefix, "" or ending in "/"
	registryID   *string // the account in the URL's host; nil is the account of the credentials
	api          api
	data         *generic.Provider
	tokens       *tokens
	create       ecr.CreateRepositoryInput // the settings of the repositories Lajstrom creates
	forceDelete  bool
	createOnPush bool
	catalog      bool
	now          func() time.Time
	log          *slog.Logger

	// known holds the Lajstrom names of the repositories that exist, with the time until
	// which EnsureRepository believes it without asking ECR.
	known sync.Map
}

// newProvider returns the Provider for cfg on the ECR API client, with the account the URL
// names ("" for none) and the clock.
func newProvider(cfg config.Registry, log *slog.Logger, client api, account string, now func() time.Time) (*Provider, error) {
	if log == nil {
		log = slog.Default()
	}
	p := &Provider{
		name:        cfg.Name,
		prefix:      normalisePrefix(cfg.Prefix),
		api:         client,
		create:      createSettings(cfg.ECR),
		forceDelete: cfg.ECR.ForceDelete,
		now:         now,
		log:         log.With("registry", cfg.Name),
	}
	if account != "" {
		p.registryID = aws.String(account)
	}
	p.tokens = &tokens{api: client, now: now, log: p.log}
	data := cfg
	data.Auth = config.RegistryAuth{}
	data.Capabilities = capabilities(cfg.Capabilities, p.create.ImageTagMutability)
	p.createOnPush, p.catalog = *data.Capabilities.CreateOnPush, *data.Capabilities.Catalog
	g, err := generic.NewWithOptions(data, log, generic.Options{Credential: p.tokens.credential})
	if err != nil {
		return nil, fmt.Errorf("ecr: %w", err)
	}
	p.data = g
	return p, nil
}

// capabilities returns the overrides of the data path: registries[].capabilities, and ECR's
// own where they leave a capability unset. What the generic provider probes (referrers) or
// assumes (deleteManifest, learnt from the first 405; blobRedirect, ECR's 307 to S3) is left
// to it: to be measured by #19, as aws/containers-roadmap#2783 reported a 405 for a referrer
// push.
func capabilities(o config.CapabilityOverrides, mutability types.ImageTagMutability) config.CapabilityOverrides {
	orDefault := func(v *bool, d bool) *bool {
		if v != nil {
			return v
		}
		return &d
	}
	// A push to a repository that does not exist fails: EnsureRepository creates it.
	// createOnPush: true says that something else does (a creation template with
	// CREATE_ON_PUSH, or repositories created beforehand), and EnsureRepository asks nothing.
	o.CreateOnPush = orDefault(o.CreateOnPush, false)
	// ECR has no _catalog; DescribeRepositories is the catalog of ListRepositories.
	o.Catalog = orDefault(o.Catalog, true)
	// The repositories Lajstrom creates refuse to re-point a tag unless they are MUTABLE*; a
	// repository created otherwise keeps its own setting.
	o.ImmutableTags = orDefault(o.ImmutableTags, strings.HasPrefix(string(mutability), "IMMUTABLE"))
	if o.MaxBlobSize == 0 {
		o.MaxBlobSize = maxLayerSize
	}
	return o
}

// createSettings is the CreateRepositoryInput of the repositories Lajstrom creates, without
// their name: the tag mutability and encryption of registries[].ecr, IMMUTABLE and AES256
// when they are empty.
func createSettings(e config.ECR) ecr.CreateRepositoryInput {
	in := ecr.CreateRepositoryInput{
		ImageTagMutability: types.ImageTagMutability(cmp.Or(e.ImageTagMutability, string(types.ImageTagMutabilityImmutable))),
		EncryptionConfiguration: &types.EncryptionConfiguration{
			EncryptionType: types.EncryptionType(cmp.Or(e.Encryption, string(types.EncryptionTypeAes256))),
		},
	}
	if e.KMSKey != "" {
		in.EncryptionConfiguration.KmsKey = aws.String(e.KMSKey)
	}
	for _, f := range e.ImageTagMutabilityExclusions {
		in.ImageTagMutabilityExclusionFilters = append(in.ImageTagMutabilityExclusionFilters, types.ImageTagMutabilityExclusionFilter{
			FilterType: types.ImageTagMutabilityExclusionFilterTypeWildcard,
			Filter:     aws.String(f),
		})
	}
	return in
}

// normalisePrefix returns the instance prefix as it is prepended to names, "" or "<prefix>/",
// as the generic provider does.
func normalisePrefix(prefix string) string {
	prefix = strings.TrimSuffix(strings.TrimPrefix(prefix, "/"), "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

// Name returns registries[].name.
func (p *Provider) Name() string { return p.name }

// Capabilities are the generic provider's, with ECR's (capabilities) where the configuration
// sets none.
func (p *Provider) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	return p.data.Capabilities(ctx)
}

// Repository returns the data path of the repository name, the generic provider's.
func (p *Provider) Repository(ctx context.Context, name string) (provider.Repository, error) {
	return p.data.Repository(ctx, name)
}

// OpenBlob is the generic provider's.
func (p *Provider) OpenBlob(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (provider.Blob, error) {
	return p.data.OpenBlob(ctx, name, d, rangeHeader, redirect)
}

// StatBlob is the generic provider's.
func (p *Provider) StatBlob(ctx context.Context, name string, d digest.Digest) (provider.Blob, error) {
	return p.data.StatBlob(ctx, name, d)
}

// Ping implements provider.Pinger: GET /v2/ with the authorization token.
func (p *Provider) Ping(ctx context.Context) error { return p.data.Ping(ctx) }
