package ecr

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

const (
	// lifecycleSample is the most repositories below the instance prefix whose lifecycle policy
	// the check at start reads, spread evenly over their names in order.
	lifecycleSample = 20
	// lifecycleTimeout bounds the check at start.
	lifecycleTimeout = time.Minute
)

// lifecyclePolicy is what the check reads of an ECR lifecycle policy (lifecycle policy
// properties, ECR user guide).
type lifecyclePolicy struct {
	Rules []lifecycleRule `json:"rules"`
}

// lifecycleRule is one rule of a lifecycle policy.
type lifecycleRule struct {
	RulePriority int    `json:"rulePriority"`
	Description  string `json:"description"`
	Selection    struct {
		TagStatus      string   `json:"tagStatus"`      // tagged, untagged or any
		TagPatternList []string `json:"tagPatternList"` // '*' is any run of characters
		TagPrefixList  []string `json:"tagPrefixList"`
		CountType      string   `json:"countType"`
		CountUnit      string   `json:"countUnit"`
		CountNumber    int      `json:"countNumber"`
	} `json:"selection"`
	Action struct {
		Type string `json:"type"` // expire, or transition to the archive
	} `json:"action"`
}

// reservedTags are the tags of Lajstrom's own forms, each with an example, that a rule can take
// besides the versions' (docs/spec/manifest-profile.md §4.2, §6 and §8;
// docs/spec/proxy-and-group.md §11.6).
var reservedTags = []struct{ kind, example string }{
	{"keep-tags _keep.sha256.*", profile.KeepTag(digest.FromString("lajstrom"))},
	{"npm journal _j*", profile.JournalTag(1)},
	{"hashed version tags _h*", profile.Tag(strings.Repeat("x", 129), "")},
	{"referrers tag schema sha256-*", "sha256-" + digest.FromString("lajstrom").Encoded()},
}

// catches says whether the rule can expire or archive what Lajstrom needs, and which of
// reservedTags it selects. A rule of untagged images only cannot: ECR keeps an image that a
// manifest list references while the list exists, and a referrer while its subject exists
// (lifecycle policy evaluation rules, ECR user guide). Any tagged rule can, for a version's tag
// can be any string; a status the check does not know is taken as any.
func (r lifecycleRule) catches() (bool, []string) {
	var kinds []string
	switch r.Selection.TagStatus {
	case "untagged":
		return false, nil
	case "tagged":
		for _, t := range reservedTags {
			if r.selects(t.example) {
				kinds = append(kinds, t.kind)
			}
		}
	default:
		for _, t := range reservedTags {
			kinds = append(kinds, t.kind)
		}
	}
	return true, kinds
}

// selects reports whether an image with the tag can be selected by the rule's tag lists: ECR
// selects an image whose tags match every entry of the list, and an image of Lajstrom's can
// hold several tags (a keep-tag and an upstream's tag), so one entry that matches is enough.
func (r lifecycleRule) selects(tag string) bool {
	for _, p := range r.Selection.TagPatternList {
		re := "^" + strings.ReplaceAll(regexp.QuoteMeta(p), `\*`, ".*") + "$"
		if ok, err := regexp.MatchString(re, tag); ok || err != nil {
			return true
		}
	}
	for _, p := range r.Selection.TagPrefixList {
		if strings.HasPrefix(tag, p) {
			return true
		}
	}
	return false
}

// attrs are the rule's fields for a log line.
func (r lifecycleRule) attrs(repos []string) []any {
	s := r.Selection
	a := []any{"repositories", repos, "rulePriority", r.RulePriority}
	if r.Description != "" {
		a = append(a, "description", r.Description)
	}
	a = append(a, "tagStatus", s.TagStatus)
	if len(s.TagPatternList) > 0 {
		a = append(a, "tagPatternList", s.TagPatternList)
	}
	if len(s.TagPrefixList) > 0 {
		a = append(a, "tagPrefixList", s.TagPrefixList)
	}
	a = append(a, "countType", s.CountType, "countNumber", s.CountNumber)
	if s.CountUnit != "" {
		a = append(a, "countUnit", s.CountUnit)
	}
	return append(a, "action", r.Action.Type)
}

// checkLifecycle reads the lifecycle policies of at most lifecycleSample repositories below the
// instance prefix and logs every rule that can expire or archive what Lajstrom needs, once per
// policy text (docs/registries/ecr.md, Lifecycle policies). It only advises: what fails is
// logged once and ends it, ecr:GetLifecyclePolicy refused included. Capabilities starts it once,
// in the background; it closes lifecycleDone.
func (p *Provider) checkLifecycle(ctx context.Context) {
	defer close(p.lifecycleDone)
	ctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()
	names, err := p.ListRepositories(ctx, "")
	switch {
	case errors.Is(err, provider.ErrUnsupported):
		p.log.InfoContext(ctx, "ECR lifecycle policies not checked: capabilities.catalog is false, and the check lists the repositories below the prefix")
		return
	case err != nil:
		p.log.WarnContext(ctx, "ECR lifecycle policies not checked: the repositories below the prefix could not be listed", "error", err.Error())
		return
	case len(names) == 0:
		p.log.DebugContext(ctx, "ECR lifecycle policies not checked: no repository below the prefix yet")
		return
	}
	sampled := spread(names, lifecycleSample)
	texts, repos, err := p.lifecyclePolicies(ctx, sampled)
	switch {
	case isCode(err, "AccessDeniedException"):
		p.log.InfoContext(ctx, "ECR lifecycle policies not checked: the credentials may not call ecr:GetLifecyclePolicy, "+
			"an optional action of the IAM policy (docs/registries/ecr.md)", "action", "ecr:GetLifecyclePolicy")
		return
	case err != nil:
		p.log.WarnContext(ctx, "ECR lifecycle policies not checked", "error", err.Error())
		return
	}
	with := 0
	for _, text := range texts {
		with += len(repos[text])
		p.logPolicy(ctx, text, repos[text])
	}
	p.log.InfoContext(ctx, "ECR lifecycle policies checked", "repositories", len(names), "sampled", len(sampled), "withPolicy", with)
}

// lifecyclePolicies returns the distinct lifecycle policy texts of the repositories names, in
// the order first met, each with the names that have it; a repository without one is left out.
// It stops at the first error.
func (p *Provider) lifecyclePolicies(ctx context.Context, names []string) ([]string, map[string][]string, error) {
	var texts []string
	repos := map[string][]string{}
	for _, name := range names {
		out, err := p.api.GetLifecyclePolicy(ctx, &ecr.GetLifecyclePolicyInput{RepositoryName: aws.String(p.prefix + name), RegistryId: p.registryID})
		switch {
		case isCode(err, "LifecyclePolicyNotFoundException"), isCode(err, "RepositoryNotFoundException"):
			continue
		case err != nil:
			return nil, nil, translate("get lifecycle policy "+name, err)
		}
		text := aws.ToString(out.LifecyclePolicyText)
		if _, ok := repos[text]; !ok {
			texts = append(texts, text)
		}
		repos[text] = append(repos[text], name)
	}
	return texts, repos, nil
}

// logPolicy logs each rule of the lifecycle policy text of the repositories repos: at warning
// one that can expire or archive what Lajstrom needs, at info one of untagged images only.
func (p *Provider) logPolicy(ctx context.Context, text string, repos []string) {
	var policy lifecyclePolicy
	if err := json.Unmarshal([]byte(text), &policy); err != nil {
		p.log.WarnContext(ctx, "an ECR lifecycle policy below the prefix could not be read: check by hand that it spares what Lajstrom needs "+
			"(docs/operations.md, The registry's own rules)", "repositories", repos, "error", err.Error())
		return
	}
	for _, r := range policy.Rules {
		harms, kinds := r.catches()
		if !harms {
			p.log.InfoContext(ctx, "an ECR lifecycle rule below the prefix takes untagged images only: ECR keeps a referrer while its "+
				"subject exists and a manifest an index lists while the index exists, so it spares what Lajstrom writes", r.attrs(repos)...)
			continue
		}
		p.log.WarnContext(ctx, "an ECR lifecycle rule below the prefix can expire or archive what Lajstrom needs: a version's tag "+
			"can be any string, and reservedTags names the other tags of Lajstrom's it selects; keep the registry's rules off "+
			"Lajstrom's repositories, or rules with tagStatus untagged only (docs/operations.md, The registry's own rules)",
			append(r.attrs(repos), "reservedTags", kinds)...)
	}
}

// spread returns at most n of names, evenly spaced from the first.
func spread(names []string, n int) []string {
	if len(names) <= n {
		return names
	}
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, names[i*len(names)/n])
	}
	return out
}
