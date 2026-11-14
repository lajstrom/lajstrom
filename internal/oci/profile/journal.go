package profile

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The journal of a package (docs/spec/manifest-profile.md §8, ADR 0012): what changes on a
// package after its versions are published, as one manifest per change under the tag _j<seq> of
// the package's OCI repository, never rewritten. npm journals a package's dist-tags and the
// deprecation of its versions, pypi the yank of its versions (PEP 592, #81).

// The artifact types of the journal's entries.
const (
	ArtifactTypeNpmDistTags    = "application/vnd.lajstrom.npm.dist-tags.v1"
	ArtifactTypeNpmDeprecation = "application/vnd.lajstrom.npm.deprecation.v1"
	ArtifactTypePypiYank       = "application/vnd.lajstrom.pypi.yank.v1"
)

// The annotations of an npm journal entry, besides lajstrom.schema, .format, .repo, .name,
// .uploaded-by and org.opencontainers.image.created.
const (
	AnnotationNpmSeq     = "lajstrom.npm.seq"
	AnnotationNpmOp      = "lajstrom.npm.op"
	AnnotationNpmTag     = "lajstrom.npm.tag"
	AnnotationNpmVersion = "lajstrom.npm.version"
	AnnotationNpmMessage = "lajstrom.npm.message"
)

// The annotations of a pypi journal entry, besides those every entry has: the reason of a yank
// is optional.
const (
	AnnotationPypiSeq     = "lajstrom.pypi.seq"
	AnnotationPypiOp      = "lajstrom.pypi.op"
	AnnotationPypiVersion = "lajstrom.pypi.version"
	AnnotationPypiReason  = "lajstrom.pypi.reason"
)

// The ops of a journal entry: add and rm of a dist-tag, deprecate and undeprecate of an npm
// version, yank and unyank of a pypi version.
const (
	JournalAdd         = "add"
	JournalRm          = "rm"
	JournalDeprecate   = "deprecate"
	JournalUndeprecate = "undeprecate"
	JournalYank        = "yank"
	JournalUnyank      = "unyank"
)

// The kinds of what an entry sets, as the index keeps them (journal_state.kind).
const (
	JournalKindDistTag    = "dist-tag"
	JournalKindDeprecated = "deprecated"
	JournalKindYanked     = "yanked"
)

// LatestTag is the dist-tag an add moves and nothing removes.
const LatestTag = "latest"

// MaxJournalMessage is the longest deprecation message and the longest yank reason, in bytes.
const MaxJournalMessage = 4096

// journalTagPrefix starts the tag of every journal entry; Escape never writes _j.
const journalTagPrefix = "_j"

var (
	// distTagName is the grammar of a dist-tag: never begins with a digit, so never a version.
	distTagName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)
	// journalTag is the tag of an entry: _j and at least eight digits.
	journalTag = regexp.MustCompile(`^_j[0-9]{8,}$`)
)

// journalKeys are the annotation keys of one format's journal: an entry's seq and op, and the
// keys of a change's Tag, Version and Message, "" for a field the format's journal has not.
type journalKeys struct{ seq, op, tag, version, message string }

// journalFormats are the formats that have a journal, and the keys of each.
var journalFormats = map[string]journalKeys{
	"npm":  {AnnotationNpmSeq, AnnotationNpmOp, AnnotationNpmTag, AnnotationNpmVersion, AnnotationNpmMessage},
	"pypi": {AnnotationPypiSeq, AnnotationPypiOp, "", AnnotationPypiVersion, AnnotationPypiReason},
}

// opRule is what an op has: the keys it requires and those it may have.
type opRule struct{ required, optional []string }

// journalType is an artifact type of the journal: the format whose journal it is, and its ops.
type journalType struct {
	format string
	ops    map[string]opRule
}

// journalTypes are the artifact types of journal entries.
var journalTypes = map[string]journalType{
	ArtifactTypeNpmDistTags: {"npm", map[string]opRule{
		JournalAdd: {required: []string{AnnotationNpmTag, AnnotationNpmVersion}},
		JournalRm:  {required: []string{AnnotationNpmTag}},
	}},
	ArtifactTypeNpmDeprecation: {"npm", map[string]opRule{
		JournalDeprecate:   {required: []string{AnnotationNpmVersion, AnnotationNpmMessage}},
		JournalUndeprecate: {required: []string{AnnotationNpmVersion}},
	}},
	ArtifactTypePypiYank: {"pypi", map[string]opRule{
		JournalYank:   {required: []string{AnnotationPypiVersion}, optional: []string{AnnotationPypiReason}},
		JournalUnyank: {required: []string{AnnotationPypiVersion}},
	}},
}

// keysOf returns the keys of the journal whose artifact type is t; the zero value for another.
func keysOf(t string) journalKeys { return journalFormats[journalTypes[t].format] }

// ErrJournal is the error of a journal entry that breaks the rules of §8.
var ErrJournal = fmt.Errorf("%w journal entry", ErrInvalid)

// IsDistTag reports whether tag is a well-formed dist-tag.
func IsDistTag(tag string) bool { return distTagName.MatchString(tag) }

// IsJournalArtifactType reports whether t is the artifact type of a journal entry.
func IsJournalArtifactType(t string) bool { _, ok := journalTypes[t]; return ok }

// HasJournal reports whether the packages of format have a journal: npm and pypi.
func HasJournal(format string) bool { _, ok := journalFormats[format]; return ok }

// JournalFormat returns the format whose journal has entries of the artifact type t, and false
// when t is no journal's.
func JournalFormat(t string) (string, bool) {
	jt, ok := journalTypes[t]
	return jt.format, ok
}

// JournalTag returns the tag of the entry seq: _j and seq zero-padded to eight digits.
func JournalTag(seq int64) string { return fmt.Sprintf("%s%08d", journalTagPrefix, seq) }

// JournalSeq returns the seq of a journal tag, and false for any other tag, also for a tag
// that is not the JournalTag of its number (_j000000001).
func JournalSeq(tag string) (int64, bool) {
	if !journalTag.MatchString(tag) {
		return 0, false
	}
	seq, err := strconv.ParseInt(strings.TrimPrefix(tag, journalTagPrefix), 10, 64)
	if err != nil || seq < 1 || JournalTag(seq) != tag {
		return 0, false
	}
	return seq, true
}

// JournalChange is the change one entry makes: the fields of format.JournalOp.
type JournalChange struct {
	ArtifactType string // ArtifactTypeNpmDistTags, ArtifactTypeNpmDeprecation or ArtifactTypePypiYank
	Op           string // JournalAdd, JournalRm, JournalDeprecate, JournalUndeprecate, JournalYank or JournalUnyank
	Tag          string // lajstrom.npm.tag: add and rm
	Version      string // lajstrom.npm.version: add, deprecate and undeprecate; lajstrom.pypi.version: yank and unyank
	Message      string // lajstrom.npm.message: deprecate; lajstrom.pypi.reason: yank, optional
}

// Check checks the change against §8: an op of its artifact type with the keys the op requires,
// perhaps those it may have, and none other; a dist-tag of the grammar (rm of latest is
// refused), a version, and a deprecation message or a yank reason of 1 to MaxJournalMessage
// bytes of UTF-8. Errors wrap ErrJournal.
func (c JournalChange) Check() error {
	jt, ok := journalTypes[c.ArtifactType]
	rule, known := jt.ops[c.Op]
	if !ok || !known {
		return fmt.Errorf("%w: op %q of artifactType %q", ErrJournal, c.Op, c.ArtifactType)
	}
	keys := journalFormats[jt.format]
	if err := c.checkKeys(jt.format, keys, rule); err != nil {
		return err
	}
	switch {
	case c.Tag != "" && !IsDistTag(c.Tag):
		return fmt.Errorf("%w: the dist-tag %q is not a letter and up to 127 letters, digits, '.', '_' or '-'", ErrJournal, c.Tag)
	case c.Op == JournalRm && c.Tag == LatestTag:
		return fmt.Errorf("%w: the dist-tag latest is never removed", ErrJournal)
	case c.Message != "" && (len(c.Message) > MaxJournalMessage || !utf8.ValidString(c.Message)):
		return fmt.Errorf("%w: %s is 1 to %d bytes of UTF-8", ErrJournal, keys.message, MaxJournalMessage)
	}
	return nil
}

// checkKeys checks that the change has the keys rule requires, perhaps those it may have, and no
// other of the journal of format, whose keys are keys.
func (c JournalChange) checkKeys(format string, keys journalKeys, rule opRule) error {
	if c.Tag != "" && keys.tag == "" {
		return fmt.Errorf("%w: op %s with a dist-tag, which the %s journal has not", ErrJournal, c.Op, format)
	}
	for k, v := range c.values(keys) {
		switch required := slices.Contains(rule.required, k); {
		case required && v == "":
			return fmt.Errorf("%w: op %s without %s", ErrJournal, c.Op, k)
		case !required && v != "" && !slices.Contains(rule.optional, k):
			return fmt.Errorf("%w: op %s with %s, which is not a key of the op", ErrJournal, c.Op, k)
		}
	}
	return nil
}

// values are the annotations of the change's keys in the journal of keys, empty ones included;
// a field the journal has not is left out.
func (c JournalChange) values(keys journalKeys) map[string]string {
	v := map[string]string{keys.version: c.Version, keys.message: c.Message}
	if keys.tag != "" {
		v[keys.tag] = c.Tag
	}
	return v
}

// JournalEntry is one entry of a package's journal as its manifest has it.
type JournalEntry struct {
	JournalChange
	Format     string    // lajstrom.format: the artifact type's, npm or pypi
	Repo       string    // lajstrom.repo
	Name       string    // lajstrom.name, the package as its format writes it
	Seq        int64     // lajstrom.<format>.seq
	UploadedBy string    // lajstrom.uploaded-by; "" when none
	Created    time.Time // org.opencontainers.image.created
}

// Attrs returns the entry's lajstrom.npm.* or lajstrom.pypi.* annotations: seq, op and the keys
// of the op, what the index keeps of an entry (journal.attrs).
func (e JournalEntry) Attrs() map[string]string {
	keys := keysOf(e.ArtifactType)
	attrs := map[string]string{keys.seq: strconv.FormatInt(e.Seq, 10), keys.op: e.Op}
	for k, v := range e.values(keys) {
		if v != "" {
			attrs[k] = v
		}
	}
	return attrs
}

// BuildJournal returns the manifest of the entry e (§8): its artifact type, the empty config, one
// empty layer, and the annotations. e is checked as ParseJournal checks a manifest.
func BuildJournal(e JournalEntry) (ocispec.Manifest, error) {
	ann := e.Attrs()
	ann[AnnotationSchema] = SchemaVersion
	ann[AnnotationFormat], ann[AnnotationRepo], ann[AnnotationName] = e.Format, e.Repo, e.Name
	if e.UploadedBy != "" {
		ann[AnnotationUploadedBy] = e.UploadedBy
	}
	if !e.Created.IsZero() {
		ann[ocispec.AnnotationCreated] = e.Created.UTC().Format(time.RFC3339)
	}
	var m ocispec.Manifest
	m.SchemaVersion = 2
	m.MediaType = ocispec.MediaTypeImageManifest
	m.ArtifactType = e.ArtifactType
	m.Config = EmptyConfig
	m.Layers = []ocispec.Descriptor{EmptyConfig}
	m.Annotations = ann
	if _, err := ParseJournal(m); err != nil {
		return ocispec.Manifest{}, err
	}
	return m, nil
}

// ParseJournal reads a journal entry from its manifest and checks the rules of §8 that the
// manifest alone can answer: an OCI image manifest without a subject, an artifact type of the
// journal, the empty config and one empty layer, lajstrom.schema 1, lajstrom.format of the
// artifact type, lajstrom.repo and .name, a decimal seq from 1 without leading zeros, the change
// (Check) in the keys of the format, an RFC 3339 created, and no lajstrom.version nor
// lajstrom.variant. Whether the tag is JournalTag(seq), the format, the repo and the name those
// of the repository is the caller's to check. Errors wrap ErrJournal and are joined.
func ParseJournal(m ocispec.Manifest) (JournalEntry, error) {
	a := m.Annotations
	keys := keysOf(m.ArtifactType)
	e := JournalEntry{Format: a[AnnotationFormat], Repo: a[AnnotationRepo], Name: a[AnnotationName], UploadedBy: a[AnnotationUploadedBy],
		JournalChange: changeIn(m.ArtifactType, keys, a)}
	errs := journalShape(m)
	bad := func(f string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: "+f, append([]any{ErrJournal}, args...)...))
	}
	if e.Repo == "" || e.Name == "" {
		bad("%s and %s are required", AnnotationRepo, AnnotationName)
	}
	seq, err := strconv.ParseInt(a[keys.seq], 10, 64)
	if e.Seq = seq; err != nil || seq < 1 || strconv.FormatInt(seq, 10) != a[keys.seq] {
		bad("%s is %q, a decimal from 1", keys.seq, a[keys.seq])
	}
	if err := e.Check(); err != nil {
		errs = append(errs, err)
	}
	if e.Created, err = parseCreated(a[ocispec.AnnotationCreated]); err != nil {
		bad("%s: %v", ocispec.AnnotationCreated, err)
	}
	if err := errors.Join(errs...); err != nil {
		return JournalEntry{}, err
	}
	return e, nil
}

// changeIn reads the change of an entry of artifact type t from the annotations a, in the keys
// of its journal: a's own, or the attrs the index keeps.
func changeIn(t string, keys journalKeys, a map[string]string) JournalChange {
	c := JournalChange{ArtifactType: t}
	if keys == (journalKeys{}) {
		return c // no journal's: Check refuses the artifact type
	}
	c.Op, c.Version, c.Message = a[keys.op], a[keys.version], a[keys.message]
	if keys.tag != "" {
		c.Tag = a[keys.tag]
	}
	return c
}

// journalShape checks what every entry has whatever its change: an OCI image manifest without a
// subject, the empty config and one empty layer, lajstrom.schema 1 and the .format of the
// artifact type, and no lajstrom.version nor .variant.
func journalShape(m ocispec.Manifest) []error {
	var errs []error
	bad := func(f string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: "+f, append([]any{ErrJournal}, args...)...))
	}
	if m.SchemaVersion != 2 || m.MediaType != ocispec.MediaTypeImageManifest {
		bad("schemaVersion %d, mediaType %q: an OCI image manifest", m.SchemaVersion, m.MediaType)
	}
	if m.Subject != nil {
		bad("an entry has no subject")
	}
	if !sameDescriptor(m.Config, EmptyConfig) || len(m.Layers) != 1 || !sameDescriptor(m.Layers[0], EmptyConfig) {
		bad("the config and the one layer of an entry are the empty descriptor")
	}
	a := m.Annotations
	if a[AnnotationSchema] != SchemaVersion {
		bad("%s is %q, want %q", AnnotationSchema, a[AnnotationSchema], SchemaVersion)
	}
	if f, ok := JournalFormat(m.ArtifactType); ok && a[AnnotationFormat] != f {
		bad("%s is %q, an entry of %s is of the %s journal", AnnotationFormat, a[AnnotationFormat], m.ArtifactType, f)
	}
	for _, k := range []string{AnnotationVersion, AnnotationVariant} {
		if _, ok := a[k]; ok {
			bad("an entry has no %s", k)
		}
	}
	return errs
}

// sameDescriptor reports whether d is the descriptor want by media type, digest and size.
func sameDescriptor(d, want ocispec.Descriptor) bool {
	return d.MediaType == want.MediaType && d.Digest == want.Digest && d.Size == want.Size
}

// JournalChangeOf reads the change and the seq of an entry from the artifact type and the attrs
// the index keeps (JournalEntry.Attrs), checked as Check checks a change.
func JournalChangeOf(artifactType string, attrs map[string]string) (JournalChange, int64, error) {
	keys := keysOf(artifactType)
	c := changeIn(artifactType, keys, attrs)
	if err := c.Check(); err != nil {
		return JournalChange{}, 0, err
	}
	seq, err := strconv.ParseInt(attrs[keys.seq], 10, 64)
	if err != nil || seq < 1 {
		return JournalChange{}, 0, fmt.Errorf("%w: %s is %q", ErrJournal, keys.seq, attrs[keys.seq])
	}
	return c, seq, nil
}

// JournalValue is what the fold holds of one key: the value and the seq of the entry that set it.
type JournalValue struct {
	Value string
	Seq   int64
}

// JournalState is the fold of a package's journal (§8, Replay): an npm package's dist-tags by
// name and its deprecation messages by version, a pypi package's yank reasons by version ("" for
// a yank without one), each with the entry that set it.
type JournalState map[JournalKey]JournalValue

// JournalKey is one key of the state: a dist-tag (JournalKindDistTag), a version's deprecation
// (JournalKindDeprecated) or a version's yank (JournalKindYanked).
type JournalKey struct {
	Kind string
	Key  string
}

// Apply folds the change of the entry seq into s: add sets the dist-tag to the version, rm
// removes it, deprecate sets the version's message, undeprecate clears it, yank sets the
// version's yank with its reason, unyank clears it.
func (s JournalState) Apply(seq int64, c JournalChange) {
	switch c.Op {
	case JournalAdd:
		s[JournalKey{JournalKindDistTag, c.Tag}] = JournalValue{c.Version, seq}
	case JournalRm:
		delete(s, JournalKey{JournalKindDistTag, c.Tag})
	case JournalDeprecate:
		s[JournalKey{JournalKindDeprecated, c.Version}] = JournalValue{c.Message, seq}
	case JournalUndeprecate:
		delete(s, JournalKey{JournalKindDeprecated, c.Version})
	case JournalYank:
		s[JournalKey{JournalKindYanked, c.Version}] = JournalValue{c.Message, seq}
	case JournalUnyank:
		delete(s, JournalKey{JournalKindYanked, c.Version})
	}
}

// Keys returns the keys of s ordered by kind and key, as the index lists them.
func (s JournalState) Keys() []JournalKey {
	return slices.SortedFunc(maps.Keys(s), func(a, b JournalKey) int {
		return strings.Compare(a.Kind+"\x00"+a.Key, b.Kind+"\x00"+b.Key)
	})
}
