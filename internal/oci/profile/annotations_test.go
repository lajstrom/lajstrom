package profile

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// debAnnotationMap is the annotation map of the §1 example plus an uploader and provenance.
func debAnnotationMap() map[string]string {
	return map[string]string{
		AnnotationSchema:          "1",
		AnnotationFormat:          "apt",
		AnnotationRepo:            "ubuntu-internal",
		AnnotationName:            "nginx",
		AnnotationVersion:         "1:1.18.0-6ubuntu14.4",
		AnnotationVariant:         "jammy/main/amd64",
		AnnotationDebArch:         "amd64",
		AnnotationDebSuite:        "jammy",
		AnnotationDebComponent:    "main",
		AnnotationUploadedBy:      "user:ci",
		ocispec.AnnotationCreated: "2026-11-06T10:00:00Z",
		ocispec.AnnotationSource:  "https://example.com/nginx.git",
	}
}

// debAnnotations is debAnnotationMap in typed form.
func debAnnotations() Annotations {
	return Annotations{
		Format:     "apt",
		Repo:       "ubuntu-internal",
		Name:       "nginx",
		Version:    "1:1.18.0-6ubuntu14.4",
		Variant:    "jammy/main/amd64",
		Created:    time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC),
		UploadedBy: "user:ci",
		Attrs: map[string]string{
			AnnotationDebArch:      "amd64",
			AnnotationDebSuite:     "jammy",
			AnnotationDebComponent: "main",
		},
		Extra: map[string]string{ocispec.AnnotationSource: "https://example.com/nginx.git"},
	}
}

func TestAnnotationsRoundTrip(t *testing.T) {
	m := debAnnotationMap()
	a, err := ParseAnnotations(m)
	if err != nil {
		t.Fatal(err)
	}
	if want := debAnnotations(); !reflect.DeepEqual(a, want) {
		t.Errorf("ParseAnnotations = %+v, want %+v", a, want)
	}
	if got := a.Map(); !maps.Equal(got, m) {
		t.Errorf("Map() = %v, want %v", got, m)
	}
	// Optional fields absent: no variant, no uploader, no other keys.
	minimal := Annotations{Format: "raw", Repo: "assets", Name: "a/b", Version: "1", Created: time.Unix(0, 0).UTC()}
	back, err := ParseAnnotations(minimal.Map())
	if err != nil || !reflect.DeepEqual(back, minimal) {
		t.Errorf("ParseAnnotations(Map()) = %+v, %v, want %+v", back, err, minimal)
	}
}

func TestParseAnnotationsErrors(t *testing.T) {
	required := []string{AnnotationSchema, AnnotationFormat, AnnotationRepo, AnnotationName, AnnotationVersion, ocispec.AnnotationCreated}
	for _, key := range required {
		m := debAnnotationMap()
		delete(m, key)
		_, err := ParseAnnotations(m)
		if !errors.Is(err, ErrAnnotation) || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), key+" is missing") {
			t.Errorf("without %s: %v, want ErrAnnotation naming it", key, err)
		}
		m[key] = ""
		if _, err := ParseAnnotations(m); !errors.Is(err, ErrAnnotation) || !strings.Contains(err.Error(), key+" is empty") {
			t.Errorf("empty %s: %v, want ErrAnnotation naming it", key, err)
		}
	}
	// All problems at once.
	_, err := ParseAnnotations(nil)
	for _, key := range required {
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("ParseAnnotations(nil) = %v, want it to name %s", err, key)
		}
	}
	for _, tc := range []struct{ key, value, want string }{
		{AnnotationSchema, "2", `lajstrom.schema is "2"`},
		{ocispec.AnnotationCreated, "2026-11-06", "RFC 3339"},
		{ocispec.AnnotationCreated, "2026-11-06 10:00:00Z", "RFC 3339"},
		{ocispec.AnnotationCreated, "yesterday", "RFC 3339"},
		{ocispec.AnnotationCreated, "9999-12-31T23:59:59-01:00", "outside 0000-9999"},
	} {
		m := debAnnotationMap()
		m[tc.key] = tc.value
		if _, err := ParseAnnotations(m); !errors.Is(err, ErrAnnotation) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%q: %v, want ErrAnnotation containing %q", tc.key, tc.value, err, tc.want)
		}
	}
}

func TestParseAnnotationsCreated(t *testing.T) {
	m := debAnnotationMap()
	m[ocispec.AnnotationCreated] = "2026-11-06T12:00:00.5+02:00"
	a, err := ParseAnnotations(m)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 6, 10, 0, 0, 5e8, time.UTC)
	if !a.Created.Equal(want) || a.Created.Location() != time.UTC {
		t.Errorf("Created = %v, want %v in UTC", a.Created, want)
	}
	// Map writes whole seconds in UTC.
	if got := a.Map()[ocispec.AnnotationCreated]; got != "2026-11-06T10:00:00Z" {
		t.Errorf("Map()[created] = %q", got)
	}
}

func TestAnnotationsMapTypedFieldsWin(t *testing.T) {
	a := debAnnotations()
	a.Variant, a.UploadedBy, a.Created = "", "", time.Time{}
	a.Attrs = map[string]string{AnnotationVariant: "x", AnnotationSchema: "9", AnnotationDebArch: "amd64"}
	a.Extra = map[string]string{ocispec.AnnotationCreated: "junk", AnnotationRepo: "other"}
	want := map[string]string{
		AnnotationSchema:  "1",
		AnnotationFormat:  "apt",
		AnnotationRepo:    "ubuntu-internal",
		AnnotationName:    "nginx",
		AnnotationVersion: "1:1.18.0-6ubuntu14.4",
		AnnotationDebArch: "amd64",
	}
	if got := a.Map(); !maps.Equal(got, want) {
		t.Errorf("Map() = %v, want %v", got, want)
	}
	if a.Attrs[AnnotationVariant] != "x" {
		t.Error("Map modified Attrs")
	}
}
