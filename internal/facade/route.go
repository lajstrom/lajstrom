package facade

import "strings"

// routeKind is what a path below /v2/<name>/ addresses.
type routeKind int

const (
	routeManifest  routeKind = iota + 1 // <name>/manifests/<ref>
	routeBlob                           // <name>/blobs/<digest>
	routeUploads                        // <name>/blobs/uploads/
	routeUpload                         // <name>/blobs/uploads/<id>
	routeTags                           // <name>/tags/list
	routeReferrers                      // <name>/referrers/<digest>
)

// route is a parsed path below /v2/.
type route struct {
	kind routeKind
	name string // everything before the fixed segments
	arg  string // the reference, digest or upload id; "" for the others
}

// parseRoute splits a path below /v2/ into the name and the route. ServeMux patterns cannot
// put a wildcard of several segments before fixed ones, so the split is by hand: the fixed
// segments are matched at the end, and the name is everything before them (a name may itself
// have a segment called blobs or manifests). False when no route matches.
func parseRoute(path string) (route, bool) {
	segs := strings.Split(path, "/")
	n := len(segs)
	name := func(k int) string { return strings.Join(segs[:n-k], "/") }
	switch {
	case n >= 4 && segs[n-3] == "blobs" && segs[n-2] == "uploads":
		if segs[n-1] == "" {
			return route{kind: routeUploads, name: name(3)}, true
		}
		return route{kind: routeUpload, name: name(3), arg: segs[n-1]}, true
	case n < 3:
		return route{}, false
	case segs[n-2] == "blobs" && segs[n-1] == "uploads":
		return route{kind: routeUploads, name: name(2)}, true
	case segs[n-2] == "tags" && segs[n-1] == "list":
		return route{kind: routeTags, name: name(2)}, true
	case segs[n-2] == "manifests":
		return route{kind: routeManifest, name: name(2), arg: segs[n-1]}, true
	case segs[n-2] == "blobs":
		return route{kind: routeBlob, name: name(2), arg: segs[n-1]}, true
	case segs[n-2] == "referrers":
		return route{kind: routeReferrers, name: name(2), arg: segs[n-1]}, true
	}
	return route{}, false
}
