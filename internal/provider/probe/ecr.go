package probe

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/ecr"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// The probe of an ECR registry (#19). /v2 is the same API as any other registry's, answered
// with the authorization token that the ECR API hands out for the SDK's credentials, and the
// run measures it as it measures the others. Unless a repository creation template with
// CREATE_ON_PUSH covers the scratch prefix, ECR creates no repository on push: "Create on push"
// measures that first, untouched, and then the run creates each repository before its first
// write with the ECR API, as the provider does, and deletes those at the end.

// repoAPI creates and deletes repositories with a registry's own API: ECR's (ecr.Provider).
// The names are below the instance prefix.
type repoAPI interface {
	EnsureRepository(ctx context.Context, name string) error
	DeleteRepository(ctx context.Context, name string) error
}

// backend is what a run uses of the registry's provider besides its own requests to /v2.
type backend struct {
	// provider returns a provider of the registry, which logs to log: the one whose probe fills
	// "What the probe finds", and the two of the replica cell.
	provider func(log *slog.Logger) (provider.Provider, error)
	// cred answers the challenges of /v2; nil is registries[].auth.
	cred auth.CredentialFunc
	// repos creates and deletes repositories; nil for a registry without such an API.
	repos repoAPI
}

// backendOf returns the backend of cfg: the generic provider, or for type ecr the ECR provider,
// its token and its repository API. The ECR provider deletes a repository with the images in
// it (ecr.forceDelete): the run deletes only those it created itself, in a scratch prefix that
// held nothing.
func backendOf(cfg config.Registry, log *slog.Logger) (backend, error) {
	if cfg.Type != ecr.Type {
		return backend{provider: func(log *slog.Logger) (provider.Provider, error) { return generic.New(cfg, log) }}, nil
	}
	cfg.ECR.ForceDelete = true
	p, err := ecr.New(cfg, log)
	if err != nil {
		return backend{}, fmt.Errorf("probe: %w", err)
	}
	return backend{
		provider: func(log *slog.Logger) (provider.Provider, error) { return ecr.New(cfg, log) },
		cred:     p.Credential,
		repos:    p,
	}, nil
}

// ensure creates the repository repo, below the scratch prefix, before the run's first write to
// it, when viaAPI is set; once per repository.
func (r *run) ensure(ctx context.Context, repo string) error {
	if !r.viaAPI || slices.Contains(r.made, repo) {
		return nil
	}
	if err := r.backend.repos.EnsureRepository(ctx, r.rel+repo); err != nil {
		return fmt.Errorf("the registry's API did not create the repository %s: %w", r.name(repo), err)
	}
	r.made = append(r.made, repo)
	return nil
}

// below returns the repository of the full name, below the scratch prefix.
func (r *run) below(name string) string { return strings.TrimPrefix(name, r.scratch) }

// deleteMade deletes the repositories the run created with the registry's API, after the
// cleanup emptied them, and returns those it could not delete, with why.
func (r *run) deleteMade(ctx context.Context) []string {
	var left []string
	for _, repo := range slices.Backward(r.made) {
		if err := r.backend.repos.DeleteRepository(ctx, r.rel+repo); err != nil {
			left = append(left, fmt.Sprintf("repository %s (%s)", r.name(repo), err.Error()))
		}
	}
	return left
}
