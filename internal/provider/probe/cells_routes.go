package probe

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// The results of the row "Routing words in a name".
const (
	// RoutingOK: every HEAD and the GET of the blob found it, in every name.
	RoutingOK = "ok"
	// RoutingMisroutes: a HEAD or the GET of a blob in a name with a routing word missed it,
	// while none in the plain name did.
	RoutingMisroutes = "misroutes"
	// RoutingRefused: the registry did not take the push of a name with a routing word.
	RoutingRefused = "refused"
)

// routingDir is the directory of the row's names below the scratch prefix.
const routingDir = "route/"

// routingNames are the repositories of the row "Routing words in a name": a plain name, then
// names with a component `manifests` or `referrers`, or the components `tags/list`, which end
// routes of the distribution API, and names that end in a build route of Quay's web UI. Harbor
// v2.15.4 answers the HEAD of a blob in the first ones 404 or 400 at random
// (goharbor/harbor#24363), which is why ADR 0017 escapes the words; Quay's master refuses the
// token request of the first push of the others, 400 NAME_INVALID (quay/quay@77b369a), which is
// capabilities.buildRoutes (provider.BuildRoute). The names are raw: the row measures the
// registry, not Lajstrom's escaping.
var routingNames = []string{
	routingDir + "x", routingDir + "x/manifests/y", routingDir + "x/manifests",
	routingDir + "x/referrers/y", routingDir + "x/tags/list", routingDir + "x/build", routingDir + "x/build/y",
}

// RoutingWords is what the row "Routing words in a name" measured.
type RoutingWords struct {
	// Result is RoutingOK, RoutingMisroutes or RoutingRefused.
	Result string `json:"result"`
	// Heads is how many times the blob of each name was asked for by HEAD.
	Heads int           `json:"heads"`
	Names []RoutingName `json:"names"`
}

// RoutingName is what the registry answered in one name of the row.
type RoutingName struct {
	// Name is the repository's full name; Control marks the plain name.
	Name    string `json:"name"`
	Control bool   `json:"control,omitempty"`
	// Refused says which push the registry refused, and how; Head and Get are then empty.
	Refused string `json:"refused,omitempty"`
	// Head counts the answers to the HEADs of the image's layer by status; Get is the status of
	// its GET.
	Head map[int]int `json:"head,omitempty"`
	Get  int         `json:"get,omitempty"`
}

// misses counts the HEADs that did not find the blob.
func (n RoutingName) misses() int {
	k := 0
	for s, c := range n.Head {
		if !foundBlob(s) {
			k += c
		}
	}
	return k
}

// foundBlob reports whether an answer to a HEAD or GET of a blob found it: 200, or a redirect
// to its bytes (a registry on S3).
func foundBlob(status int) bool {
	return status == http.StatusOK || status/100 == 3
}

// measureRoutingWords measures "Routing words in a name": an image in each of routingNames,
// its layer asked for by HEAD Options.RoutingHeads times and by GET once. A plain name that is
// refused, or whose blob is missed, fails the measurement: the words are not the cause then.
func (r *run) measureRoutingWords(ctx context.Context) error {
	w := &RoutingWords{Heads: r.opts.RoutingHeads}
	for i, repo := range routingNames {
		n, err := r.routingName(ctx, repo)
		if err != nil {
			return err
		}
		if i == 0 {
			n.Control = true
			switch {
			case n.Refused != "":
				return fmt.Errorf("the plain name %s: %s", n.Name, n.Refused)
			case n.misses() > 0 || !foundBlob(n.Get):
				return fmt.Errorf("the plain name %s: blob `HEAD` %s, `GET` %d", n.Name, tally(n.Head, false), n.Get)
			}
		}
		w.Names = append(w.Names, n)
	}
	r.found.buildRoutesRefused, r.found.buildRoutesTaken = buildRoutes(w.Names)
	w.Result = routingResult(w.Names)
	r.report.RoutingWords = w
	r.set("routingWords", routingVerdict(w, r.scratch+routingDir))
	return nil
}

// routingName pushes an image to repo, the empty config and a layer of its own under the tag
// v1, then asks for the layer by HEAD and by GET. A token endpoint that refuses the push's
// scope, as Quay's does for a build route, refuses the name.
func (r *run) routingName(ctx context.Context, repo string) (RoutingName, error) {
	n := RoutingName{Name: r.name(repo)}
	note := "routing words, " + repo
	layer := []byte("lajstrom registry probe " + r.id + ": " + note)
	for _, b := range []struct {
		what string
		data []byte
	}{{"the empty blob", []byte("{}")}, {"the layer", layer}} {
		_, a, err := r.pushBlob(ctx, repo, b.data)
		if refusal, ok := tokenRefusal(err); ok {
			n.Refused = refusal + " at the token request of the push of " + b.what
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if !a.ok(http.StatusCreated) {
			n.Refused = a.refusal() + " to the push of " + b.what
			return n, nil
		}
	}
	d := digest.FromBytes(layer)
	m := r.layerManifest(note, ocispec.Descriptor{MediaType: "application/octet-stream", Digest: d, Size: int64(len(layer))})
	a, err := r.pushManifest(ctx, repo, "v1", m)
	if err != nil {
		return n, err
	}
	if !a.ok(http.StatusCreated, http.StatusOK) {
		n.Refused = a.refusal() + " to the push of the manifest"
		return n, nil
	}
	target := r.path(repo, "blobs/"+d.String())
	n.Head = map[int]int{}
	for range r.opts.RoutingHeads {
		a, err := r.send(ctx, http.MethodHead, target)
		if err != nil {
			return n, err
		}
		n.Head[a.status]++
	}
	if a, err = r.get(ctx, target, nil); err != nil {
		return n, err
	}
	n.Get = a.status
	return n, nil
}

// tokenRefusal describes the refusal of a token endpoint that err holds, "400 `NAME_INVALID`",
// and reports whether it holds one: the auth client's error when the endpoint answered the
// challenge of a request with a 4xx.
func tokenRefusal(err error) (string, bool) {
	var resp *errcode.ErrorResponse
	if !errors.As(err, &resp) || resp.StatusCode/100 != 4 {
		return "", false
	}
	if len(resp.Errors) > 0 && resp.Errors[0].Code != "" {
		return fmt.Sprintf("%d `%s`", resp.StatusCode, resp.Errors[0].Code), true
	}
	return fmt.Sprintf("a %d", resp.StatusCode), true
}

// buildRoutes reports whether the registry refused, with NAME_INVALID, every name of names that
// ends in a build route (provider.BuildRoute), and whether it took every one: "Overrides needed"
// compares them with capabilities.buildRoutes.
func buildRoutes(names []RoutingName) (refused, taken bool) {
	refused, taken = true, true
	seen := false
	for _, n := range names {
		if !provider.BuildRoute(n.Name) {
			continue
		}
		seen = true
		refused = refused && strings.Contains(n.Refused, "`NAME_INVALID`")
		taken = taken && n.Refused == ""
	}
	return refused && seen, taken && seen
}

// routingResult is the row's result: misroutes when a blob was missed in a name, refused when
// a name was refused and none missed, ok otherwise.
func routingResult(names []RoutingName) string {
	result := RoutingOK
	for _, n := range names {
		switch {
		case n.Refused != "":
			result = RoutingRefused
		case n.misses() > 0 || !foundBlob(n.Get):
			return RoutingMisroutes
		}
	}
	return result
}

// routingVerdict is the cell "Routing words in a name", the names shown below trim.
func routingVerdict(w *RoutingWords, trim string) string {
	var missed, gets, refused []string
	for _, n := range w.Names {
		name := "`" + strings.TrimPrefix(n.Name, trim) + "`"
		if n.Refused != "" {
			refused = append(refused, name+" "+n.Refused)
			continue
		}
		if n.misses() > 0 {
			missed = append(missed, name+" "+tally(n.Head, true))
		}
		if !foundBlob(n.Get) {
			gets = append(gets, fmt.Sprintf("%s %d", name, n.Get))
		}
	}
	switch w.Result {
	case RoutingMisroutes:
		var parts []string
		if len(missed) > 0 {
			parts = append(parts, fmt.Sprintf("blob `HEAD` in %s of %d", strings.Join(missed, ", "), w.Heads))
		}
		if len(gets) > 0 {
			parts = append(parts, "blob `GET` in "+strings.Join(gets, ", "))
		} else {
			parts = append(parts, "`GET` "+statuses(w.Names, true))
		}
		if len(refused) > 0 {
			parts = append(parts, "refused: "+strings.Join(refused, ", "))
		}
		return "**misroutes**: " + strings.Join(parts, "; ")
	case RoutingRefused:
		return fmt.Sprintf("**refused**: %s; the others: %d blob `HEAD`s each, all %s",
			strings.Join(refused, ", "), w.Heads, statuses(w.Names, false))
	}
	return fmt.Sprintf("ok: %d blob `HEAD`s in each name, all %s; `GET` %s", w.Heads, statuses(w.Names, false), statuses(w.Names, true))
}

// routingDetail lists what each name of the row answered, for the lines after the table.
func routingDetail(w *RoutingWords, trim string) string {
	parts := make([]string, 0, len(w.Names))
	for _, n := range w.Names {
		name := "`" + strings.TrimPrefix(n.Name, trim) + "`"
		if n.Refused != "" {
			parts = append(parts, name+" refused: "+n.Refused)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s `HEAD` %s, `GET` %d", name, tally(n.Head, false), n.Get))
	}
	return strings.Join(parts, "; ")
}

// tally writes the counts of statuses in their order, "4×200 16×404"; with missesOnly, of
// the statuses that did not find the blob.
func tally(counts map[int]int, missesOnly bool) string {
	var parts []string
	for _, s := range slices.Sorted(maps.Keys(counts)) {
		if !missesOnly || !foundBlob(s) {
			parts = append(parts, fmt.Sprintf("%d×%d", counts[s], s))
		}
	}
	return strings.Join(parts, " ")
}

// statuses lists the distinct statuses of the HEADs, or of the GETs, in the names that were
// pushed: "200", "200 or 307".
func statuses(names []RoutingName, get bool) string {
	seen := map[int]bool{}
	for _, n := range names {
		switch {
		case n.Refused != "":
		case get:
			seen[n.Get] = true
		default:
			for s := range n.Head {
				seen[s] = true
			}
		}
	}
	var out []string
	for _, s := range slices.Sorted(maps.Keys(seen)) {
		out = append(out, strconv.Itoa(s))
	}
	return strings.Join(out, " or ")
}
