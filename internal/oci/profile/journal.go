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

// The journal of an npm package (docs/spec/manifest-profile.md §8, ADR 0012): what changes on a
// package, its dist-tags and the deprecation of its versions, as one manifest per change under
// the tag _j<seq> of the package's OCI repository, never rewritten.

// The artifact types of the journal's entries.
const (
	ArtifactTypeNpmDistTags    = "application/vnd.lajstrom.npm.dist-tags.v1"
	ArtifactTypeNpmDeprecation = "application/vnd.lajstrom.npm.deprecation.v1"
)

// The annotations of a journal entry, besides lajstrom.schema, .format, .repo, .name,
// .uploaded-by and org.opencontainers.image.created.
const (
	AnnotationNpmSeq     = "lajstrom.npm.seq"
	AnnotationNpmOp      = "lajstrom.npm.op"
	AnnotationNpmTag     = "lajstrom.npm.tag"
	AnnotationNpmVersion = "lajstrom.npm.version"
	AnnotationNpmMessage = "lajstrom.npm.message"
)

// The ops of a journal entry: add and rm of a dist-tag, deprecate and undeprecate of a version.
const (
	JournalAdd         = "add"
	JournalRm          = "rm"
	JournalDeprecate   = "deprecate"
	JournalUndeprecate = "undeprecate"
)

// The kinds of what an entry sets, as the index keeps them (journal_state.kind).
const (
	JournalKindDistTag    = "dist-tag"
	JournalKindDeprecated = "deprecated"
)

// LatestTag is the dist-tag an add moves and nothing removes.
const LatestTag = "latest"

// MaxJournalMessage is the longest deprecation message, in bytes.
const MaxJournalMessage = 4096

// journalTagPrefix starts the tag of every journal entry; Escape never writes _j.
const journalTagPrefix = "_j"

// journalFormat is the lajstrom.format of every entry: the journal is npm's.
const journalFormat = "npm"

var (
	// distTagName is the grammar of a dist-tag: never begins with a digit, so never a version.
	distTagName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)
	// journalTag is the tag of an entry: _j and at least eight digits.
	journalTag = regexp.MustCompile(`^_j[0-9]{8,}$`)
)

// opKeys are the keys of each op, by artifact type and op.
var opKeys = map[string]map[string][]string{
	ArtifactTypeNpmDistTags: {
		JournalAdd: {AnnotationNpmTag, AnnotationNpmVersion},
		JournalRm:  {AnnotationNpmTag},
	},
	ArtifactTypeNpmDeprecation: {
		JournalDeprecate:   {AnnotationNpmVersion, AnnotationNpmMessage},
		JournalUndeprecate: {AnnotationNpmVersion},
	},
}

// ErrJournal is the error of a journal entry that breaks the rules of §8.
var ErrJournal = fmt.Errorf("%w journal entry", ErrInvalid)

// IsDistTag reports whether tag is a well-formed dist-tag.
func IsDistTag(tag string) bool { return distTagName.MatchString(tag) }

// IsJournalArtifactType reports whether t is the artifact type of a journal entry.
func IsJournalArtifactType(t string) bool { _, ok := opKeys[t]; return ok }

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
	ArtifactType string // ArtifactTypeNpmDistTags or ArtifactTypeNpmDeprecation
	Op           string // JournalAdd, JournalRm, JournalDeprecate or JournalUndeprecate
	Tag          string // lajstrom.npm.tag: add and rm
	Version      string // lajstrom.npm.version: add, deprecate and undeprecate
	Message      string // lajstrom.npm.message: deprecate
}

// Check checks the change against §8: an op of its artifact type with the keys of the op and
// none other, a dist-tag of the grammar (rm of latest is refused), a version, and a message of
// 1 to MaxJournalMessage bytes of UTF-8. Errors wrap ErrJournal.
func (c JournalChange) Check() error {
	keys, ok := opKeys[c.ArtifactType][c.Op]
	if !ok {
		return fmt.Errorf("%w: op %q of artifactType %q", ErrJournal, c.Op, c.ArtifactType)
	}
	for k, v := range c.values() {
		switch key := slices.Contains(keys, k); {
		case key && v == "":
			return fmt.Errorf("%w: op %s without %s", ErrJournal, c.Op, k)
		case !key && v != "":
			return fmt.Errorf("%w: op %s with %s, which is not a key of the op", ErrJournal, c.Op, k)
		}
	}
	switch {
	case c.Tag != "" && !IsDistTag(c.Tag):
		return fmt.Errorf("%w: the dist-tag %q is not a letter and up to 127 letters, digits, '.', '_' or '-'", ErrJournal, c.Tag)
	case c.Op == JournalRm && c.Tag == LatestTag:
		return fmt.Errorf("%w: the dist-tag latest is never removed", ErrJournal)
	case c.Message != "" && (len(c.Message) > MaxJournalMessage || !utf8.ValidString(c.Message)):
		return fmt.Errorf("%w: a deprecation message is 1 to %d bytes of UTF-8", ErrJournal, MaxJournalMessage)
	}
	return nil
}

// values are the annotations of the change's keys, empty ones included.
func (c JournalChange) values() map[string]string {
	return map[string]string{AnnotationNpmTag: c.Tag, AnnotationNpmVersion: c.Version, AnnotationNpmMessage: c.Message}
}

// JournalEntry is one entry of a package's journal as its manifest has it.
type JournalEntry struct {
	JournalChange
	Format     string    // lajstrom.format: npm
	Repo       string    // lajstrom.repo
	Name       string    // lajstrom.name, the package as npm writes it
	Seq        int64     // lajstrom.npm.seq
	UploadedBy string    // lajstrom.uploaded-by; "" when none
	Created    time.Time // org.opencontainers.image.created
}

// Attrs returns the entry's lajstrom.npm.* annotations: seq, op and the keys of the op, what the
// index keeps of an entry (journal.attrs).
func (e JournalEntry) Attrs() map[string]string {
	attrs := map[string]string{AnnotationNpmSeq: strconv.FormatInt(e.Seq, 10), AnnotationNpmOp: e.Op}
	for k, v := range e.values() {
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
// (Check), an RFC 3339 created, and no lajstrom.version nor lajstrom.variant. Whether the tag is
// JournalTag(seq), the repo and the name those of the repository is the caller's to check.
// Errors wrap ErrJournal and are joined.
func ParseJournal(m ocispec.Manifest) (JournalEntry, error) {
	a := m.Annotations
	e := JournalEntry{Format: a[AnnotationFormat], Repo: a[AnnotationRepo], Name: a[AnnotationName], UploadedBy: a[AnnotationUploadedBy],
		JournalChange: JournalChange{ArtifactType: m.ArtifactType, Op: a[AnnotationNpmOp], Tag: a[AnnotationNpmTag],
			Version: a[AnnotationNpmVersion], Message: a[AnnotationNpmMessage]}}
	errs := journalShape(m)
	bad := func(f string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: "+f, append([]any{ErrJournal}, args...)...))
	}
	if e.Repo == "" || e.Name == "" {
		bad("%s and %s are required", AnnotationRepo, AnnotationName)
	}
	seq, err := strconv.ParseInt(a[AnnotationNpmSeq], 10, 64)
	if e.Seq = seq; err != nil || seq < 1 || strconv.FormatInt(seq, 10) != a[AnnotationNpmSeq] {
		bad("%s is %q, a decimal from 1", AnnotationNpmSeq, a[AnnotationNpmSeq])
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

// journalShape checks what every entry has whatever its change: an OCI image manifest without a
// subject, the empty config and one empty layer, lajstrom.schema 1 and .format npm, and no
// lajstrom.version nor .variant.
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
	if a[AnnotationFormat] != journalFormat {
		bad("%s is %q, the journal is npm's", AnnotationFormat, a[AnnotationFormat])
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
	c := JournalChange{ArtifactType: artifactType, Op: attrs[AnnotationNpmOp], Tag: attrs[AnnotationNpmTag],
		Version: attrs[AnnotationNpmVersion], Message: attrs[AnnotationNpmMessage]}
	seq, err := strconv.ParseInt(attrs[AnnotationNpmSeq], 10, 64)
	if err != nil || seq < 1 {
		return JournalChange{}, 0, fmt.Errorf("%w: %s is %q", ErrJournal, AnnotationNpmSeq, attrs[AnnotationNpmSeq])
	}
	return c, seq, c.Check()
}

// JournalValue is what the fold holds of one key: the value and the seq of the entry that set it.
type JournalValue struct {
	Value string
	Seq   int64
}

// JournalState is the fold of a package's journal (§8, Replay): its dist-tags by name and its
// deprecation messages by version, each with the entry that set it.
type JournalState map[JournalKey]JournalValue

// JournalKey is one key of the state: a dist-tag (JournalKindDistTag) or a version's deprecation
// (JournalKindDeprecated).
type JournalKey struct {
	Kind string
	Key  string
}

// Apply folds the change of the entry seq into s: add sets the dist-tag to the version, rm
// removes it, deprecate sets the version's message, undeprecate clears it.
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
	}
}

// Keys returns the keys of s ordered by kind and key, as the index lists them.
func (s JournalState) Keys() []JournalKey {
	return slices.SortedFunc(maps.Keys(s), func(a, b JournalKey) int {
		return strings.Compare(a.Kind+"\x00"+a.Key, b.Kind+"\x00"+b.Key)
	})
}
