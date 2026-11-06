package profile

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// annotationPrefix starts every Lajstrom annotation key.
const annotationPrefix = "lajstrom."

// Annotations are the manifest annotations of docs/spec/manifest-profile.md §3 in typed form.
type Annotations struct {
	// Format is lajstrom.format, the protocol format: raw, pypi, apt, maven. Required.
	Format string
	// Repo is lajstrom.repo. Required.
	Repo string
	// Name is lajstrom.name, the package name in the format's own spelling. Required.
	Name string
	// Version is lajstrom.version, exactly as the format spells it. Required.
	Version string
	// Variant is lajstrom.variant; empty when the format has none.
	Variant string
	// Created is org.opencontainers.image.created, in UTC. Required.
	Created time.Time
	// UploadedBy is lajstrom.uploaded-by; empty when absent.
	UploadedBy string
	// Attrs holds every other lajstrom.* annotation by its full key, for example
	// "lajstrom.deb.arch". Nil when there are none.
	Attrs map[string]string
	// Extra holds every annotation outside lajstrom.* except org.opencontainers.image.created,
	// for example org.opencontainers.image.source. Nil when there are none.
	Extra map[string]string
}

// ParseAnnotations reads manifest annotations. The required keys (lajstrom.schema, .format,
// .repo, .name, .version and org.opencontainers.image.created) must be present and non-empty,
// lajstrom.schema must be SchemaVersion, and created must be an RFC 3339 time, which is
// converted to UTC. An empty lajstrom.variant or lajstrom.uploaded-by counts as absent. Errors
// wrap ErrAnnotation, name the key, and are joined so that all problems are reported at once.
func ParseAnnotations(m map[string]string) (Annotations, error) {
	var errs []error
	required := func(key string) string {
		v, ok := m[key]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%w: %s is missing", ErrAnnotation, key))
		case v == "":
			errs = append(errs, fmt.Errorf("%w: %s is empty", ErrAnnotation, key))
		}
		return v
	}
	if v := required(AnnotationSchema); v != "" && v != SchemaVersion {
		errs = append(errs, fmt.Errorf("%w: %s is %q, want %q", ErrAnnotation, AnnotationSchema, v, SchemaVersion))
	}
	a := Annotations{
		Format:     required(AnnotationFormat),
		Repo:       required(AnnotationRepo),
		Name:       required(AnnotationName),
		Version:    required(AnnotationVersion),
		Variant:    m[AnnotationVariant],
		UploadedBy: m[AnnotationUploadedBy],
	}
	if v := required(ocispec.AnnotationCreated); v != "" {
		t, err := parseCreated(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: %s: %w", ErrAnnotation, ocispec.AnnotationCreated, err))
		}
		a.Created = t
	}
	if len(errs) > 0 {
		return Annotations{}, errors.Join(errs...)
	}
	a.Attrs, a.Extra = splitRest(m)
	return a, nil
}

// parseCreated parses an RFC 3339 time and converts it to UTC. It rejects times whose UTC form
// cannot be written back as RFC 3339 (a year outside 0000-9999 after the conversion).
func parseCreated(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("not an RFC 3339 time: %w", err)
	}
	t = t.UTC()
	if y := t.Year(); y < 0 || y > 9999 {
		return time.Time{}, fmt.Errorf("%q is year %d in UTC, outside 0000-9999", v, y)
	}
	return t, nil
}

// isTyped reports whether key is one of the annotations Annotations has a field for.
func isTyped(key string) bool {
	switch key {
	case AnnotationSchema, AnnotationFormat, AnnotationRepo, AnnotationName, AnnotationVersion,
		AnnotationVariant, AnnotationUploadedBy, ocispec.AnnotationCreated:
		return true
	}
	return false
}

// splitRest sorts the annotations without a typed field into lajstrom.* (attrs) and the rest
// (extra). Each map is nil when empty.
func splitRest(m map[string]string) (attrs, extra map[string]string) {
	for k, v := range m {
		switch {
		case isTyped(k):
		case strings.HasPrefix(k, annotationPrefix):
			if attrs == nil {
				attrs = map[string]string{}
			}
			attrs[k] = v
		default:
			if extra == nil {
				extra = map[string]string{}
			}
			extra[k] = v
		}
	}
	return attrs, extra
}

// Map returns the annotations as a manifest annotation map, the inverse of ParseAnnotations.
// lajstrom.schema is always SchemaVersion; Created is written in RFC 3339 UTC with whole seconds;
// empty fields and a zero Created are omitted. The typed fields win over the same key in Attrs or
// Extra. The result is a new map.
func (a Annotations) Map() map[string]string {
	m := make(map[string]string, len(a.Extra)+len(a.Attrs)+8)
	maps.Copy(m, a.Extra)
	maps.Copy(m, a.Attrs)
	created := ""
	if !a.Created.IsZero() {
		created = a.Created.UTC().Format(time.RFC3339)
	}
	for k, v := range map[string]string{
		AnnotationSchema:          SchemaVersion,
		AnnotationFormat:          a.Format,
		AnnotationRepo:            a.Repo,
		AnnotationName:            a.Name,
		AnnotationVersion:         a.Version,
		AnnotationVariant:         a.Variant,
		AnnotationUploadedBy:      a.UploadedBy,
		ocispec.AnnotationCreated: created,
	} {
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return m
}
