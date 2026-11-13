package ecr

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"

	"github.com/lajstrom/lajstrom/internal/config"
)

// clock is a time the test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeECR is the ECR API of the unit tests: tokens that live for lifetime on the clock,
// repositories in a map, DescribeRepositories paginated by pageSize. An error knob set fails
// its operation. Every call is counted by operation name.
type fakeECR struct {
	clock    *clock
	lifetime time.Duration // of the tokens issued
	gate     chan struct{} // when set, GetAuthorizationToken waits for it to close

	mu       sync.Mutex
	calls    map[string]int
	tokenErr error
	issued   int

	createErr error
	repos     map[string]ecr.CreateRepositoryInput // name → how it was created

	listErr, deleteErr error
	pageSize           int            // of DescribeRepositories; 0 is one page
	images             map[string]int // name → images it holds

	policyErr error
	policies  map[string]string // name → its lifecycle policy text
}

func newFakeECR(c *clock) *fakeECR {
	f := &fakeECR{clock: c, lifetime: 12 * time.Hour, calls: map[string]int{}}
	f.repos = map[string]ecr.CreateRepositoryInput{}
	f.images = map[string]int{}
	f.policies = map[string]string{}
	return f
}

func (f *fakeECR) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

// call counts op and returns the error knob err points at.
func (f *fakeECR) call(op string, err *error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[op]++
	return *err
}

// GetAuthorizationToken waits for the gate, if any, and issues the next token.
func (f *fakeECR) GetAuthorizationToken(context.Context, *ecr.GetAuthorizationTokenInput, ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	if f.gate != nil {
		<-f.gate
	}
	if err := f.call("GetAuthorizationToken", &f.tokenErr); err != nil {
		return nil, err
	}
	out := new(ecr.GetAuthorizationTokenOutput)
	out.AuthorizationData = append(out.AuthorizationData, f.issue())
	return out, nil
}

// issue makes the next token: the n-th has the password "token-<n>" and expires lifetime
// from now on the clock.
func (f *fakeECR) issue() types.AuthorizationData {
	f.mu.Lock()
	f.issued++
	password := fmt.Sprintf("token-%d", f.issued)
	f.mu.Unlock()
	expires := f.clock.now().Add(f.lifetime)
	basic := base64.StdEncoding.EncodeToString([]byte("AWS:" + password))
	return types.AuthorizationData{ExpiresAt: &expires, AuthorizationToken: &basic}
}

// CreateRepository keeps the input of a name it does not hold, and refuses one it does.
func (f *fakeECR) CreateRepository(_ context.Context, in *ecr.CreateRepositoryInput, _ ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error) {
	err := f.call("CreateRepository", &f.createErr)
	if err == nil && !f.keep(*in) {
		err = &types.RepositoryAlreadyExistsException{Message: in.RepositoryName}
	}
	if err != nil {
		return nil, err
	}
	return &ecr.CreateRepositoryOutput{}, nil
}

// keep records in under its repository name, unless that name is taken.
func (f *fakeECR) keep(in ecr.CreateRepositoryInput) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := aws.ToString(in.RepositoryName)
	if _, taken := f.repos[key]; taken {
		return false
	}
	f.repos[key] = in
	return true
}

func (f *fakeECR) DescribeRepositories(_ context.Context, in *ecr.DescribeRepositoriesInput, _ ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error) {
	if err := f.call("DescribeRepositories", &f.listErr); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	names := slices.Sorted(maps.Keys(f.repos))
	start, _ := strconv.Atoi(aws.ToString(in.NextToken))
	end := len(names)
	if f.pageSize > 0 {
		end = min(start+f.pageSize, len(names))
	}
	out := &ecr.DescribeRepositoriesOutput{}
	for _, n := range names[start:end] {
		out.Repositories = append(out.Repositories, types.Repository{RepositoryName: aws.String(n)})
	}
	if end < len(names) {
		out.NextToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}

func (f *fakeECR) DeleteRepository(_ context.Context, in *ecr.DeleteRepositoryInput, _ ...func(*ecr.Options)) (*ecr.DeleteRepositoryOutput, error) {
	if err := f.call("DeleteRepository", &f.deleteErr); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.RepositoryName)
	if _, ok := f.repos[name]; !ok {
		return nil, &types.RepositoryNotFoundException{Message: aws.String("The repository with name '" + name + "' does not exist")}
	}
	if f.images[name] > 0 && !in.Force {
		return nil, &types.RepositoryNotEmptyException{Message: aws.String("The repository with name '" + name + "' still contains images")}
	}
	delete(f.repos, name)
	delete(f.images, name)
	return &ecr.DeleteRepositoryOutput{}, nil
}

// GetLifecyclePolicy answers the policy text of a repository, LifecyclePolicyNotFoundException
// for one without, and RepositoryNotFoundException for one it does not hold.
func (f *fakeECR) GetLifecyclePolicy(_ context.Context, in *ecr.GetLifecyclePolicyInput, _ ...func(*ecr.Options)) (*ecr.GetLifecyclePolicyOutput, error) {
	if err := f.call("GetLifecyclePolicy", &f.policyErr); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := aws.ToString(in.RepositoryName)
	if _, ok := f.repos[name]; !ok {
		return nil, &types.RepositoryNotFoundException{Message: aws.String("The repository with name '" + name + "' does not exist")}
	}
	text, ok := f.policies[name]
	if !ok {
		return nil, &types.LifecyclePolicyNotFoundException{Message: aws.String("Lifecycle policy does not exist for the repository with name '" + name + "'")}
	}
	return &ecr.GetLifecyclePolicyOutput{RepositoryName: in.RepositoryName, LifecyclePolicyText: aws.String(text)}, nil
}

// fakeProvider is a Provider on f with the instance prefix art/; edit changes the
// configuration first.
func fakeProvider(t *testing.T, f *fakeECR, edit func(*config.Registry)) *Provider {
	t.Helper()
	cfg := config.Registry{Name: "ecr", Type: Type, URL: "https://123456789012.dkr.ecr.us-east-1.amazonaws.com", Prefix: "art/"}
	if edit != nil {
		edit(&cfg)
	}
	p, err := newProvider(cfg, slog.New(slog.DiscardHandler), f, "123456789012", f.clock.now)
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	return p
}
