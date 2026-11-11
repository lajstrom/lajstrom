package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// npm has group repos (ADR 0013, docs/spec/http-routing.md, npm, Group): the core asks the
// members for their packuments and merges them here.
var _ format.Grouper = Plugin{}

// MergeGroup merges the packuments of kind of name of the members of a group, in member order
// (docs/spec/proxy-and-group.md §8.3, http-routing.md, npm, Group): versions the union, a
// version that several members have from the first; time per version likewise, created the
// earliest and modified the latest; each dist-tag the first member's that has it; every other
// member of the document the first member's. Each dist.tarball is under base, the group's.
func (Plugin) MergeGroup(_ context.Context, _ index.Repo, name, kind string, docs []format.MemberDocument, base string) (format.Document, error) {
	contentType, err := contentTypeOf(kind)
	if err != nil {
		return format.Document{}, err
	}
	var out map[string]json.RawMessage
	versions, distTags, times := map[string]json.RawMessage{}, map[string]json.RawMessage{}, map[string]json.RawMessage{}
	var modified, created time.Time
	for i, d := range docs {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(d.Content, &doc); err != nil || doc == nil {
			return format.Document{}, fmt.Errorf("npm: the packument of %s in %s/%s: %w", name, d.Repo.Format, d.Repo.Name, err)
		}
		if i == 0 {
			out = maps.Clone(doc)
		}
		for v, raw := range members(doc["versions"]) {
			if _, ok := versions[v]; !ok {
				versions[v] = relinked(raw, base+name+"/-/")
			}
		}
		addAbsent(distTags, members(doc["dist-tags"]))
		t := members(doc["time"])
		created = earliest(created, t["created"])
		modified = latest(latest(modified, t["modified"]), doc["modified"])
		delete(t, "created")
		delete(t, "modified")
		addAbsent(times, t)
	}
	out["versions"], out["dist-tags"] = mustJSON(versions), mustJSON(distTags)
	if _, ok := out["modified"]; ok && !modified.IsZero() {
		out["modified"] = mustJSON(modified.Format(time.RFC3339Nano))
	}
	if _, ok := out["time"]; ok {
		if !created.IsZero() {
			times["created"] = mustJSON(created.Format(time.RFC3339Nano))
		}
		if !modified.IsZero() {
			times["modified"] = mustJSON(modified.Format(time.RFC3339Nano))
		}
		out["time"] = mustJSON(times)
	}
	content, err := json.Marshal(out)
	if err != nil {
		return format.Document{}, fmt.Errorf("npm: the merged packument of %s: %w", name, err)
	}
	return format.Document{Kind: kind, Path: name, ContentType: contentType, Content: content}, nil
}

// groupAudit answers the bulk audit of a group (docs/spec/http-routing.md, npm, Audit): the
// request without the names that the group's hosted member holds, which never leave the
// server, forwarded to its first proxy member as proxyAudit forwards a bulk request, without
// the names that member's deny list refuses too; {} when no name is left or the group has no
// proxy member.
func (h *handlers) groupAudit(w http.ResponseWriter, r *http.Request, group index.Repo, body io.Reader) {
	ctx := r.Context()
	names, err := bulkNames(body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	members, err := h.deps.Index.ListGroupMembers(ctx, group.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var upstream *index.Repo
	for i, m := range members {
		switch {
		case m.Kind == index.KindHosted:
			if err := h.dropHeld(ctx, m, names); err != nil {
				h.fail(w, r, err)
				return
			}
		case m.Kind == index.KindProxy && upstream == nil:
			upstream = &members[i]
		}
	}
	if upstream == nil || len(names) == 0 {
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	core, err := h.proxyCore(*upstream)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.forwardBulk(w, r.WithContext(format.WithVia(ctx, group)), core, *upstream, names)
}

// dropHeld removes from names those that the hosted repo holds, by package path.
func (h *handlers) dropHeld(ctx context.Context, hosted index.Repo, names map[string]json.RawMessage) error {
	for name := range names {
		pkgPath, err := h.plugin.EscapeName(name)
		if err != nil {
			continue // a name npm cannot have is no package of the repo, and the upstream answers it
		}
		_, err = index.PackageAt(ctx, h.deps.Index, hosted.ID, pkgPath)
		switch {
		case err == nil:
			delete(names, name)
		case !errors.Is(err, index.ErrNotFound):
			return err
		}
	}
	return nil
}

// members reads a JSON object's members; nil for anything else.
func members(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// addAbsent adds to dst each member of src that dst does not have: the first member wins.
func addAbsent(dst, src map[string]json.RawMessage) {
	for k, v := range src {
		if _, ok := dst[k]; !ok {
			dst[k] = v
		}
	}
}

// relinked returns a version document with dist.tarball below prefix, <base><name>/-/, keeping
// the file name of the member's link.
func relinked(raw json.RawMessage, prefix string) json.RawMessage {
	var vd map[string]json.RawMessage
	if json.Unmarshal(raw, &vd) != nil || vd == nil {
		return raw
	}
	dist := members(vd["dist"])
	var link string
	if dist == nil || json.Unmarshal(dist["tarball"], &link) != nil || link == "" {
		return raw
	}
	dist["tarball"] = mustJSON(prefix + link[strings.LastIndex(link, "/")+1:])
	vd["dist"] = mustJSON(dist)
	return mustJSON(vd)
}

// earliest and latest compare t with the RFC 3339 time raw, a JSON string; anything else
// leaves t.
func earliest(t time.Time, raw json.RawMessage) time.Time {
	if v, ok := timeOf(raw); ok && (t.IsZero() || v.Before(t)) {
		return v
	}
	return t
}

func latest(t time.Time, raw json.RawMessage) time.Time {
	if v, ok := timeOf(raw); ok && v.After(t) {
		return v
	}
	return t
}

func timeOf(raw json.RawMessage) (time.Time, bool) {
	t := published(raw)
	return t, !t.IsZero()
}
