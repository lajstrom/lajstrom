package pypi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// pypi has group repos (ADR 0013, docs/spec/http-routing.md, pypi, Group): the core asks the
// members for their JSON pages of a project and merges them here. A hosted member's page is its
// own, under the scope simple/<project> (HostedScope).
var _ format.HostedScoper = Plugin{}

// HostedScope is the scope of a hosted repo's page of the project whose key is key:
// simple/<project>.
func (Plugin) HostedScope(key string) format.IndexScope {
	return format.IndexScope(scopeProjectsPath + key)
}

// MergeGroup merges the JSON pages of a project of the members of a group, in member order
// (docs/spec/proxy-and-group.md §8.3, http-routing.md, pypi, Group): files the union by file
// name, a file that several members list as the first lists it; versions the union, in order;
// the API version 1.1 when every page is of 1.1, else 1.0. Each URL is relative to the page, as
// the members write them, so it names the group the page is served for; base is not read.
func (p Plugin) MergeGroup(_ context.Context, _ index.Repo, name, kind string, docs []format.MemberDocument, _ string) (format.Document, error) {
	project, err := p.Key(name)
	if err != nil {
		return format.Document{}, err
	}
	if err := checkKind(kind); err != nil {
		return format.Document{}, err
	}
	version, files, versions := apiVersion, []json.RawMessage{}, []string{}
	names, seen := map[string]bool{}, map[string]bool{}
	for _, d := range docs {
		var page struct {
			Meta     map[string]string `json:"meta"`
			Versions []string          `json:"versions"`
			Files    []json.RawMessage `json:"files"`
		}
		if err := json.Unmarshal(d.Content, &page); err != nil {
			return format.Document{}, fmt.Errorf("pypi: the page of %s in %s/%s: %w", project, d.Repo.Format, d.Repo.Name, err)
		}
		if page.Meta["api-version"] != apiVersion {
			version = keptAPIVersion
		}
		for _, raw := range page.Files {
			var f struct {
				Filename string `json:"filename"`
			}
			if json.Unmarshal(raw, &f) != nil || names[f.Filename] {
				continue
			}
			names[f.Filename], files = true, append(files, raw)
		}
		for _, v := range page.Versions {
			if !seen[v] {
				versions, seen[v] = append(versions, v), true
			}
		}
	}
	content, err := marshal(struct {
		Meta     map[string]string `json:"meta"`
		Name     string            `json:"name"`
		Versions []string          `json:"versions"`
		Files    []json.RawMessage `json:"files"`
	}{map[string]string{"api-version": version}, project, versions, files})
	if err != nil {
		return format.Document{}, err
	}
	return format.Document{Kind: kindJSON, Path: scopeProjectsPath + project + "/", ContentType: contentTypeJSON, Content: content}, nil
}
