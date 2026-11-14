package ecr

import (
	"bytes"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/smithy-go"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if (want == nil) != (err == nil) || (want != nil && !errors.Is(err, want)) {
		t.Errorf("%s: error = %v, want %v", what, err, want)
	}
}

// TestEnsureRepository: a repository that does not exist is created with the settings of the
// configuration below the instance prefix, one that exists (created beforehand, or by another
// replica) is left alone, and either is not asked for again for knownFor.
func TestEnsureRepository(t *testing.T) {
	ctx := t.Context()
	f := newFakeECR(newClock())
	p := fakeProvider(t, f, func(c *config.Registry) {
		c.ECR = config.ECR{ImageTagMutability: "IMMUTABLE_WITH_EXCLUSION", ImageTagMutabilityExclusions: []string{"sha256-*"},
			Encryption: "KMS", KMSKey: "alias/lajstrom"}
	})
	wantErr(t, "create", p.EnsureRepository(ctx, "raw/scratch/notes"), nil)
	want := ecr.CreateRepositoryInput{
		RepositoryName:     aws.String("art/raw/scratch/notes"),
		RegistryId:         aws.String("123456789012"),
		ImageTagMutability: types.ImageTagMutabilityImmutableWithExclusion,
		ImageTagMutabilityExclusionFilters: []types.ImageTagMutabilityExclusionFilter{
			{FilterType: types.ImageTagMutabilityExclusionFilterTypeWildcard, Filter: aws.String("sha256-*")},
		},
		EncryptionConfiguration: &types.EncryptionConfiguration{EncryptionType: types.EncryptionTypeKms, KmsKey: aws.String("alias/lajstrom")},
	}
	opts := cmpopts.IgnoreUnexported(ecr.CreateRepositoryInput{}, types.ImageTagMutabilityExclusionFilter{}, types.EncryptionConfiguration{})
	if diff := cmp.Diff(want, f.repos["art/raw/scratch/notes"], opts); diff != "" {
		t.Errorf("CreateRepository input (-want +got):\n%s", diff)
	}
	wantErr(t, "again", p.EnsureRepository(ctx, "raw/scratch/notes"), nil)
	if n := f.count("CreateRepository"); n != 1 {
		t.Errorf("CreateRepository called %d times, want 1", n)
	}

	// Created beforehand: CreateRepository answers that it exists, which is success.
	f.repos["art/raw/scratch/made-by-terraform"] = ecr.CreateRepositoryInput{}
	wantErr(t, "existing", p.EnsureRepository(ctx, "raw/scratch/made-by-terraform"), nil)
	wantErr(t, "existing again", p.EnsureRepository(ctx, "raw/scratch/made-by-terraform"), nil)
	if n := f.count("CreateRepository"); n != 2 {
		t.Errorf("CreateRepository called %d times, want 2", n)
	}

	// After knownFor ECR is asked again: a repository deleted behind Lajstrom's back comes back.
	delete(f.repos, "art/raw/scratch/notes")
	f.clock.advance(knownFor)
	wantErr(t, "after knownFor", p.EnsureRepository(ctx, "raw/scratch/notes"), nil)
	if _, ok := f.repos["art/raw/scratch/notes"]; !ok || f.count("CreateRepository") != 3 {
		t.Errorf("the repository was not created again (%d calls)", f.count("CreateRepository"))
	}

	// A quota of the account is ErrLimit (#357), with a warning for the operator.
	var buf bytes.Buffer
	p.log = slog.New(slog.NewJSONHandler(&buf, nil))
	f.createErr = &types.LimitExceededException{Message: aws.String("repositories per region")}
	wantErr(t, "quota", p.EnsureRepository(ctx, "raw/scratch/other"), provider.ErrLimit)
	if log := buf.String(); !strings.Contains(log, `"level":"WARN"`) || !strings.Contains(log, "service quota") ||
		!strings.Contains(log, `"repository":"raw/scratch/other"`) {
		t.Errorf("the quota is not logged at warn with the repository: %s", log)
	}
	f.createErr = &smithy.GenericAPIError{Code: "AccessDeniedException"}
	wantErr(t, "denied", p.EnsureRepository(ctx, "raw/scratch/other"), provider.ErrUnauthorized)
	if p.EnsureRepository(ctx, "") == nil || p.DeleteRepository(ctx, "") == nil {
		t.Error("an empty name is not an error")
	}
}

// TestEnsureRepositoryCreateOnPush: with createOnPush something else creates the repositories,
// a creation template or the operator, and EnsureRepository asks ECR nothing.
func TestEnsureRepositoryCreateOnPush(t *testing.T) {
	f := newFakeECR(newClock())
	p := fakeProvider(t, f, func(c *config.Registry) { c.Capabilities.CreateOnPush = aws.Bool(true) })
	wantErr(t, "createOnPush", p.EnsureRepository(t.Context(), "raw/scratch/notes"), nil)
	if n := f.count("CreateRepository"); n != 0 {
		t.Errorf("CreateRepository called %d times, want none", n)
	}
}

// TestListRepositories: every page of DescribeRepositories, filtered by the instance prefix and
// the prefix asked for, without the instance prefix.
func TestListRepositories(t *testing.T) {
	ctx := t.Context()
	f := newFakeECR(newClock())
	f.pageSize = 2
	for _, n := range []string{"art/raw/a/x", "art/raw/b", "art/pypi/c", "other/raw/d", "art/raw/e", "art/rawish/f", "raw/g"} {
		f.repos[n] = ecr.CreateRepositoryInput{}
	}
	p := fakeProvider(t, f, nil)
	got, err := p.ListRepositories(ctx, "raw/")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"raw/a/x", "raw/b", "raw/e"}; !slices.Equal(got, want) {
		t.Errorf("ListRepositories = %q, want %q", got, want)
	}
	if n := f.count("DescribeRepositories"); n != 4 {
		t.Errorf("DescribeRepositories called %d times, want 4 pages", n)
	}

	f.listErr = &smithy.GenericAPIError{Code: "AccessDeniedException"}
	_, err = p.ListRepositories(ctx, "raw/")
	wantErr(t, "denied", err, provider.ErrUnauthorized)

	off := fakeProvider(t, f, func(c *config.Registry) { c.Capabilities.Catalog = aws.Bool(false) })
	_, err = off.ListRepositories(ctx, "raw/")
	wantErr(t, "catalog false", err, provider.ErrUnsupported)
}

// TestDeleteRepository: without forceDelete ECR refuses a repository with images (409); with
// it, the images go too; a repository that does not exist is 404.
func TestDeleteRepository(t *testing.T) {
	ctx := t.Context()
	f := newFakeECR(newClock())
	p := fakeProvider(t, f, nil)
	wantErr(t, "ensure", p.EnsureRepository(ctx, "raw/scratch/notes"), nil)
	f.images["art/raw/scratch/notes"] = 3
	wantErr(t, "not empty", p.DeleteRepository(ctx, "raw/scratch/notes"), provider.ErrConflict)

	force := fakeProvider(t, f, func(c *config.Registry) { c.ECR.ForceDelete = true })
	wantErr(t, "force", force.DeleteRepository(ctx, "raw/scratch/notes"), nil)
	wantErr(t, "gone", p.DeleteRepository(ctx, "raw/scratch/notes"), provider.ErrNotFound)

	// The delete forgets the repository: the next ensure creates it again.
	wantErr(t, "ensure after the delete", p.EnsureRepository(ctx, "raw/scratch/notes"), nil)
	if _, ok := f.repos["art/raw/scratch/notes"]; !ok {
		t.Error("EnsureRepository after DeleteRepository did not create the repository")
	}
}
