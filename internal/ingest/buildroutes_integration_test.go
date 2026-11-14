//go:build integration

package ingest_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegrationBuildRoutes publishes a new package whose OCI repository ends in a build route,
// raw/<repo>/build, against the registry of TestIntegration (#662):
//
//   - with the registry's capabilities: where they say buildRoutes (Quay, by the vendor table),
//     the upload is ErrInvalid naming the rule and nothing reaches the registry; elsewhere it is
//     published;
//   - with capabilities.buildRoutes: false, the registry answers for itself: one that creates
//     the name publishes it, and Quay's master refuses the token of the push, 400 NAME_INVALID,
//     which the upload returns as the registry's refusal;
//   - a package so published takes a new version with the registry's capabilities again: the
//     index has it, and the registry holds its repository.
func TestIntegrationBuildRoutes(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, prov := integrationEnv(t, addr)
	caps, err := prov.Capabilities(ctx)
	must(t, err)
	_, err = e.pipe.Upload(ctx, e.repo, upload("build", "1.0.0", file{"build", "first " + e.repo.Name}), alice)
	switch {
	case caps.BuildRoutes && (!errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), "ends in a build route")):
		t.Fatalf("Upload of raw/%s/build with buildRoutes: %v, want ErrInvalid naming the rule", e.repo.Name, err)
	case caps.BuildRoutes:
		t.Logf("with buildRoutes: %v", err)
		if r := e.rows(t); len(r.Packages) != 0 || len(r.Audit) != 0 {
			t.Errorf("rows written for a refused name: %+v", r)
		}
	case err != nil:
		t.Fatalf("Upload of raw/%s/build without buildRoutes: %v", e.repo.Name, err)
	default:
		return // the registry creates the name, and nothing says otherwise
	}

	off, err := generic.New(config.Registry{
		Name:   "local",
		Type:   generic.Type,
		URL:    "http://" + addr,
		Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{
			Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"),
			Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD"),
		},
		Capabilities: config.CapabilityOverrides{BuildRoutes: new(false)},
	}, slog.New(slog.DiscardHandler))
	must(t, err)
	e.pipe = ingest.New(e.options(off))
	_, err = e.pipe.Upload(ctx, e.repo, upload("build", "1.0.0", file{"build", "first " + e.repo.Name}), alice)
	var rej *provider.RejectedError
	switch {
	case errors.As(err, &rej) && rej.Code == "NAME_INVALID":
		t.Logf("buildRoutes: false, the registry refuses to create the name: %v", err)
		return
	case err != nil:
		t.Fatalf("Upload with buildRoutes: false: %v (%T), want nil or the registry's NAME_INVALID", err, err)
	}
	t.Logf("buildRoutes: false, the registry created raw/%s/build", e.repo.Name)

	e.pipe = ingest.New(e.options(prov))
	if _, err := e.pipe.Upload(ctx, e.repo, upload("build", "2.0.0", file{"build", "second " + e.repo.Name}), alice); err != nil {
		t.Errorf("Upload of 2.0.0 of the package the index has, with buildRoutes: %v", err)
	}
	if r := e.rows(t); len(r.Versions) != 2 {
		t.Errorf("versions %+v, want 1.0.0 and 2.0.0", r.Versions)
	}
}
