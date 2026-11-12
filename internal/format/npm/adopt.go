package npm

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// npm's proxy repos may become hosted repos (docs/spec/proxy-and-group.md §2.6): the dist-tags
// and the deprecations of the upstream's packuments are carried over as journal ops.
var _ format.Adopter = Plugin{}

// AdoptKind is the full packument, which has the dist-tags and each version's deprecated.
func (Plugin) AdoptKind() string { return kindPackument }

// AdoptOps returns the journal ops that give a hosted repo holding the versions held of name the
// dist-tags and the deprecations of kept, a packument of name as ParseMetadata kept it: an add
// per dist-tag whose version is held, in the order of the tags, then a deprecate per held
// version whose deprecated is a message, in the order of the versions. A dist-tag of a version
// not held, and an op the journal refuses (profile.JournalChange.Check: a tag npm would not
// take, a message too long), are left out and described in skipped. A body that is not a
// packument of name wraps format.ErrInvalid.
func (Plugin) AdoptOps(name string, kept []byte, held []string) ([]format.JournalOp, []string, error) {
	var doc struct {
		Name     string                     `json:"name"`
		DistTags map[string]json.RawMessage `json:"dist-tags"`
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(kept, &doc); err != nil {
		return nil, nil, fmt.Errorf("%w: the packument of %s: %w", format.ErrInvalid, name, err)
	}
	if doc.Name != name {
		return nil, nil, fmt.Errorf("%w: the packument of %s names %q", format.ErrInvalid, name, doc.Name)
	}
	var ops []format.JournalOp
	var skipped []string
	take := func(op format.JournalOp, what string) {
		c := profile.JournalChange{ArtifactType: op.ArtifactType, Op: op.Op, Tag: op.Tag, Version: op.Version, Message: op.Message}
		if err := c.Check(); err != nil {
			skipped = append(skipped, what+": "+err.Error())
			return
		}
		ops = append(ops, op)
	}
	for _, tag := range slices.Sorted(maps.Keys(doc.DistTags)) {
		var v string
		switch err := json.Unmarshal(doc.DistTags[tag], &v); {
		case err != nil || v == "":
			skipped = append(skipped, fmt.Sprintf("dist-tag %s: not a version", tag))
		case !slices.Contains(held, v):
			skipped = append(skipped, fmt.Sprintf("dist-tag %s: %s is not held", tag, v))
		default:
			take(format.JournalOp{ArtifactType: profile.ArtifactTypeNpmDistTags, Op: profile.JournalAdd, Tag: tag, Version: v},
				"dist-tag "+tag)
		}
	}
	for _, v := range slices.Sorted(slices.Values(held)) {
		var vd struct {
			Deprecated json.RawMessage `json:"deprecated"`
		}
		if json.Unmarshal(doc.Versions[v], &vd) != nil {
			continue
		}
		var msg string
		if json.Unmarshal(vd.Deprecated, &msg) != nil || msg == "" {
			continue // absent, false, or not a message: not deprecated
		}
		take(format.JournalOp{ArtifactType: profile.ArtifactTypeNpmDeprecation, Op: profile.JournalDeprecate, Version: v, Message: msg},
			"the deprecation of "+v)
	}
	return ops, skipped, nil
}
