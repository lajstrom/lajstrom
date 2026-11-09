package maven

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
)

// maxPOM is the largest POM the plugin reads: a large bill of materials is a few hundred KiB.
const maxPOM = 4 << 20

// DefaultPackaging is the packaging of a POM that has no packaging element.
const DefaultPackaging = "jar"

// POM is what Lajstrom reads of a project object model, POM 4.0.0: the coordinates, the
// packaging, the parent and the dependencies.
type POM struct {
	// Coordinates are the project's, groupId and version inherited from the parent when the
	// POM has none, and ${project.*} references resolved.
	Coordinates
	// Packaging is the packaging, DefaultPackaging when the POM has none: jar, pom, war,
	// maven-plugin, bundle.
	Packaging string
	// Parent is the parent, nil when the POM has none.
	Parent *Coordinates
	// Dependencies are the dependencies of the project, in order, not those of
	// dependencyManagement, of a profile or of a plugin.
	Dependencies []Dependency
}

// Dependency is a dependency of a POM as it writes it, with ${project.*} references resolved and
// any other property reference kept as written: it may be a property of a parent Lajstrom does
// not read. A field the POM leaves out is empty. The JSON keys are the POM's.
type Dependency struct {
	GroupID    string `json:"groupId" xml:"groupId"`
	ArtifactID string `json:"artifactId" xml:"artifactId"`
	Version    string `json:"version,omitempty" xml:"version"`
	Type       string `json:"type,omitempty" xml:"type"`
	Classifier string `json:"classifier,omitempty" xml:"classifier"`
	Scope      string `json:"scope,omitempty" xml:"scope"`
	Optional   string `json:"optional,omitempty" xml:"optional"`
}

// pomXML is the part of a POM that ParsePOM reads. The fields have no namespace, so that they
// match a POM in the POM 4.0.0 namespace and one in none, which old POMs are.
type pomXML struct {
	XMLName      xml.Name
	ModelVersion string          `xml:"modelVersion"`
	Parent       *coordinatesXML `xml:"parent"`
	coordinatesXML
	Packaging    string       `xml:"packaging"`
	Dependencies []Dependency `xml:"dependencies>dependency"`
}

// coordinatesXML are the coordinate elements of a project and of a parent.
type coordinatesXML struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
}

// ciFriendly are the properties of Maven's CI-friendly versions, which only the build resolves.
var ciFriendly = []string{"${revision}", "${sha1}", "${changelist}"}

// propertyRef is a property reference, ${name}.
var propertyRef = regexp.MustCompile(`\$\{[^}]*\}`)

// ParsePOM reads a POM 4.0.0 with encoding/xml: UTF-8, US-ASCII or ISO-8859-1, at most 4 MiB,
// the root element project, modelVersion 4.0.0 or none. groupId and version are the parent's
// when the project has none; artifactId is never inherited; packaging is DefaultPackaging when
// absent. References to ${project.groupId}, ${project.artifactId}, ${project.version},
// ${project.packaging} and ${project.parent.groupId}, .artifactId and .version are resolved, and
// no other property: coordinates of the project or of the parent that still hold a reference
// are refused, ${revision}, ${sha1} and ${changelist} (CI-friendly versions) with how to deploy
// the POM instead. The coordinates must be well formed and the version a release. Errors wrap
// format.ErrInvalid.
func ParsePOM(b []byte) (*POM, error) {
	if len(b) > maxPOM {
		return nil, fmt.Errorf("%w: the POM has %d bytes, at most %d", format.ErrInvalid, len(b), maxPOM)
	}
	var x pomXML
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.CharsetReader = charsetReader
	if err := dec.Decode(&x); err != nil {
		return nil, fmt.Errorf("%w: not a POM: %w", format.ErrInvalid, err)
	}
	switch {
	case x.XMLName.Local != "project":
		return nil, fmt.Errorf("%w: the root element of the POM is %q, not project", format.ErrInvalid, x.XMLName.Local)
	case strings.TrimSpace(x.ModelVersion) != "" && strings.TrimSpace(x.ModelVersion) != "4.0.0":
		return nil, fmt.Errorf("%w: the POM has modelVersion %q; Lajstrom reads POM 4.0.0", format.ErrInvalid, x.ModelVersion)
	}
	pom, err := x.resolve()
	if err != nil {
		return nil, err
	}
	if err := pom.check(); err != nil {
		return nil, fmt.Errorf("the POM: %w", err)
	}
	return pom, nil
}

// resolve inherits groupId and version from the parent, resolves the ${project.*} references
// and refuses coordinates that still hold one.
func (x *pomXML) resolve() (*POM, error) {
	own := x.coordinates()
	pom := &POM{Coordinates: own, Packaging: strings.TrimSpace(x.Packaging)}
	if x.Parent != nil {
		parent := x.Parent.coordinates()
		pom.Parent = &parent
		if pom.GroupID == "" {
			pom.GroupID = parent.GroupID
		}
		if pom.Version == "" {
			pom.Version = parent.Version
		}
	}
	if pom.Packaging == "" {
		pom.Packaging = DefaultPackaging
	}
	props := map[string]string{
		"project.groupId": pom.GroupID, "project.artifactId": pom.ArtifactID,
		"project.version": pom.Version, "project.packaging": pom.Packaging,
	}
	fields := []struct {
		what string
		v    *string
	}{{"groupId", &pom.GroupID}, {"artifactId", &pom.ArtifactID}, {"version", &pom.Version}, {"packaging", &pom.Packaging}}
	if p := pom.Parent; p != nil {
		props["project.parent.groupId"], props["project.parent.artifactId"], props["project.parent.version"] = p.GroupID, p.ArtifactID, p.Version
		fields = append(fields, []struct {
			what string
			v    *string
		}{{"parent groupId", &p.GroupID}, {"parent artifactId", &p.ArtifactID}, {"parent version", &p.Version}}...)
	}
	for _, f := range fields {
		*f.v = interpolate(*f.v, props)
		if err := unresolved(f.what, *f.v); err != nil {
			return nil, err
		}
	}
	for _, d := range x.Dependencies {
		for _, v := range []*string{&d.GroupID, &d.ArtifactID, &d.Version, &d.Type, &d.Classifier, &d.Scope, &d.Optional} {
			*v = interpolate(strings.TrimSpace(*v), props)
		}
		pom.Dependencies = append(pom.Dependencies, d)
	}
	return pom, nil
}

// coordinates returns the coordinates with the white space around each removed, as Maven reads
// them.
func (c coordinatesXML) coordinates() Coordinates {
	return Coordinates{GroupID: strings.TrimSpace(c.GroupID), ArtifactID: strings.TrimSpace(c.ArtifactID), Version: strings.TrimSpace(c.Version)}
}

// interpolate replaces the references to props in s. A value can hold a reference itself, so
// the replacement runs again, a few times at most: a reference to itself, ${project.version} as
// the version, is left in place.
func interpolate(s string, props map[string]string) string {
	for range 4 {
		next := propertyRef.ReplaceAllStringFunc(s, func(ref string) string {
			if v, ok := props[ref[2:len(ref)-1]]; ok {
				return v
			}
			return ref
		})
		if next == s {
			break
		}
		s = next
	}
	return s
}

// unresolved refuses a coordinate of the POM, named by what, that still holds a property
// reference.
func unresolved(what, v string) error {
	if !strings.Contains(v, "${") {
		return nil
	}
	ref := propertyRef.FindString(v)
	if ref == "" {
		ref = "${ without }"
	}
	for _, p := range ciFriendly {
		if strings.Contains(v, p) {
			return fmt.Errorf("%w: the %s of the POM is %q: %s is a CI-friendly version, which only the build resolves; "+
				"deploy a POM with the version written out (the flatten-maven-plugin, or the consumer POM of Maven 4)",
				format.ErrInvalid, what, v, p)
		}
	}
	return fmt.Errorf("%w: the %s of the POM is %q, with the property reference %s; Lajstrom resolves only ${project.*}, "+
		"so the POM in a repository has its coordinates written out", format.ErrInvalid, what, v, ref)
}

// check checks the grammar of the coordinates, the packaging and the parent, and refuses a
// SNAPSHOT version.
func (p *POM) check() error {
	switch {
	case p.GroupID == "":
		return fmt.Errorf("%w: no groupId, and no parent to inherit one from", format.ErrInvalid)
	case p.ArtifactID == "":
		return fmt.Errorf("%w: no artifactId", format.ErrInvalid)
	case p.Version == "":
		return fmt.Errorf("%w: no version, and no parent to inherit one from", format.ErrInvalid)
	case !artifactIDPattern.MatchString(p.Packaging):
		return fmt.Errorf("%w: packaging %q is not letters, digits, \"_\", \"-\" and \".\"", format.ErrInvalid, p.Packaging)
	}
	if err := p.Coordinates.check(); err != nil {
		return err
	}
	if p.Parent != nil {
		if err := checkGroupArtifact(p.Parent.GroupID, p.Parent.ArtifactID); err != nil {
			return fmt.Errorf("the parent: %w", err)
		}
		if !tokenPattern.MatchString(p.Parent.Version) {
			return fmt.Errorf("%w: the parent version %q is not letters, digits, \".\", \"_\", \"+\" and \"-\"", format.ErrInvalid, p.Parent.Version)
		}
	}
	return nil
}

// charsetReader decodes the encodings of a POM besides UTF-8, which encoding/xml reads itself:
// ISO-8859-1, which old POMs declare, and its subset US-ASCII. In ISO-8859-1 every byte is the
// code point of the same number.
func charsetReader(label string, r io.Reader) (io.Reader, error) {
	switch strings.ToLower(label) {
	case "utf8":
		return r, nil
	case "iso-8859-1", "iso8859-1", "iso_8859-1", "latin1", "l1", "us-ascii", "ascii":
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		var sb strings.Builder
		for _, c := range b {
			sb.WriteRune(rune(c))
		}
		return strings.NewReader(sb.String()), nil
	}
	return nil, errors.New("the encoding " + label + "; Lajstrom reads POMs in UTF-8, US-ASCII and ISO-8859-1")
}
