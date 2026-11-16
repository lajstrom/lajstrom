package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
)

// The layers of an encrypted repo (docs/spec/manifest-profile.md §11): the stored manifest is the
// client's, or the one a frontend's upload built, changed in place and nowhere else, so that the
// facade answers it again exactly as it was.
const (
	// EncryptedSuffix ends the media type of an encrypted layer.
	EncryptedSuffix = "+encrypted"
	// AnnotationEncJWE is the layer annotation of the wrapped private options.
	AnnotationEncJWE = "org.opencontainers.image.enc.keys.jwe"
	// AnnotationEncPubOpts is the layer annotation of the public options.
	AnnotationEncPubOpts = "org.opencontainers.image.enc.pubopts"
	// AnnotationEncPrefix starts every annotation of an encrypted layer; a client sends none.
	AnnotationEncPrefix = "org.opencontainers.image.enc."
)

// ErrEncryption means a manifest cannot be stored encrypted, or a stored one cannot be answered
// as the client had it: a layer the client encrypted itself, a descriptor the change could not
// undo exactly, a layer without its encryption.
var ErrEncryption = fmt.Errorf("%w encrypted manifest", ErrInvalid)

// EncryptedLayer is what the change writes into a layer descriptor: the ciphertext's digest and
// the values of the two annotations.
type EncryptedLayer struct {
	Stored  digest.Digest
	JWE     string
	PubOpts string
}

// CheckPlainLayers refuses a manifest whose layer the client encrypted itself: a media type that
// ends in EncryptedSuffix, or an annotation that starts with AnnotationEncPrefix. Every managed
// repo refuses it, encrypted or not.
func CheckPlainLayers(layers []LayerInfo) error {
	for _, l := range layers {
		if strings.HasSuffix(l.MediaType, EncryptedSuffix) {
			return fmt.Errorf("%w: the layer %s is %s: Lajstrom encrypts, a client pushes the plaintext", ErrEncryption, l.Digest, l.MediaType)
		}
		for k := range l.Annotations {
			if strings.HasPrefix(k, AnnotationEncPrefix) {
				return fmt.Errorf("%w: the layer %s has the annotation %s: Lajstrom encrypts, a client pushes the plaintext",
					ErrEncryption, l.Digest, k)
			}
		}
	}
	return nil
}

// LayerInfo is what CheckPlainLayers reads of a layer descriptor.
type LayerInfo struct {
	MediaType   string
	Digest      digest.Digest
	Annotations map[string]string
}

// EncryptManifest returns raw, the bytes of a manifest, with every layer descriptor changed in
// place (manifest-profile.md §11): its digest replaced with layers[digest].Stored, EncryptedSuffix
// written into its media type, and the two annotations added. When subject is not "", the
// subject's digest is replaced with it. Nothing else of raw changes. A layer missing from layers,
// one the client encrypted, a descriptor with a member twice, a digest written with an escape,
// annotations null or empty, and a layer with data (its bytes in the manifest) are ErrEncryption.
func EncryptManifest(raw []byte, layers map[digest.Digest]EncryptedLayer, subject digest.Digest) ([]byte, error) {
	m, err := scanManifest(raw)
	if err != nil {
		return nil, err
	}
	var edits []edit
	for _, l := range m.layers {
		switch {
		case l.mediaTypeEnd == 0 || l.digestStart == 0:
			return nil, fmt.Errorf("%w: a layer without a mediaType or a digest", ErrEncryption)
		case l.data:
			return nil, fmt.Errorf("%w: the layer %s carries its bytes in data", ErrEncryption, l.digest)
		}
		if strings.HasSuffix(l.mediaType, EncryptedSuffix) || l.encAnnotation {
			return nil, fmt.Errorf("%w: the layer %s is encrypted already", ErrEncryption, l.digest)
		}
		e, ok := layers[l.digest]
		if !ok {
			return nil, fmt.Errorf("%w: the layer %s has no ciphertext", ErrEncryption, l.digest)
		}
		members := `"` + AnnotationEncJWE + `":"` + e.JWE + `","` + AnnotationEncPubOpts + `":"` + e.PubOpts + `"`
		edits = append(edits,
			edit{l.mediaTypeEnd - 1, l.mediaTypeEnd - 1, EncryptedSuffix},
			edit{l.digestStart, l.digestEnd, `"` + e.Stored.String() + `"`})
		switch {
		case l.annotationsStart < 0:
			edits = append(edits, edit{l.end - 1, l.end - 1, `,"annotations":{` + members + `}`})
		case l.annotationsEmpty:
			return nil, fmt.Errorf("%w: the layer %s has empty annotations: leave them out", ErrEncryption, l.digest)
		default:
			edits = append(edits, edit{l.annotationsStart + 1, l.annotationsStart + 1, members + ","})
		}
	}
	if subject != "" && m.subject != nil {
		edits = append(edits, edit{m.subject.digestStart, m.subject.digestEnd, `"` + subject.String() + `"`})
	}
	return apply(raw, edits), nil
}

// DecryptManifest undoes EncryptManifest on raw, a stored manifest: plain returns the plaintext
// digest of a layer's ciphertext digest, false when it does not know it; subject, when not "",
// replaces the subject's digest. A manifest without encrypted layers comes back as it is, its
// subject replaced. A layer whose change is not exactly EncryptManifest's, or whose plaintext
// digest plain does not know, is ErrEncryption.
func DecryptManifest(raw []byte, plain func(stored digest.Digest) (digest.Digest, bool), subject digest.Digest) ([]byte, error) {
	m, err := scanManifest(raw)
	if err != nil {
		return nil, err
	}
	var edits []edit
	for _, l := range m.layers {
		if !strings.HasSuffix(l.mediaType, EncryptedSuffix) {
			continue
		}
		d, ok := plain(l.digest)
		if !ok {
			return nil, fmt.Errorf("%w: the plaintext digest of the layer %s is not known", ErrEncryption, l.digest)
		}
		e, err := l.inserted(raw)
		if err != nil {
			return nil, err
		}
		edits = append(edits,
			edit{l.mediaTypeEnd - 1 - len(EncryptedSuffix), l.mediaTypeEnd - 1, ""},
			edit{l.digestStart, l.digestEnd, `"` + d.String() + `"`}, e)
	}
	if subject != "" && m.subject != nil {
		edits = append(edits, edit{m.subject.digestStart, m.subject.digestEnd, `"` + subject.String() + `"`})
	}
	return apply(raw, edits), nil
}

// EncryptedLayers returns the ciphertext digest of every encrypted layer of raw, in order, with
// the values of its two annotations, and the digest of its subject ("" without one).
func EncryptedLayers(raw []byte) ([]EncryptedLayer, digest.Digest, error) {
	m, err := scanManifest(raw)
	if err != nil {
		return nil, "", err
	}
	var out []EncryptedLayer
	for _, l := range m.layers {
		if strings.HasSuffix(l.mediaType, EncryptedSuffix) {
			out = append(out, EncryptedLayer{Stored: l.digest, JWE: l.jwe, PubOpts: l.pubOpts})
		}
	}
	var subject digest.Digest
	if m.subject != nil {
		subject = m.subject.digest
	}
	return out, subject, nil
}

// inserted returns the edit that removes the annotations EncryptManifest added to l: the two
// members at the start of an object that has others, or the whole annotations member that it
// added last to a descriptor without one.
func (l *scannedLayer) inserted(raw []byte) (edit, error) {
	if l.annotationsStart < 0 {
		return edit{}, fmt.Errorf("%w: the encrypted layer %s has no annotations", ErrEncryption, l.digest)
	}
	members := `"` + AnnotationEncJWE + `":"` + l.jwe + `","` + AnnotationEncPubOpts + `":"` + l.pubOpts + `"`
	start := l.annotationsStart + 1
	if !bytes.HasPrefix(raw[start:], []byte(members)) {
		return edit{}, fmt.Errorf("%w: the annotations of the encrypted layer %s do not start with its encryption", ErrEncryption, l.digest)
	}
	after := start + len(members)
	switch {
	case after < len(raw) && raw[after] == ',':
		return edit{start, after + 1, ""}, nil
	case after < len(raw) && raw[after] == '}' && after+2 == l.end && l.annotationsKeyStart > 0 && raw[l.annotationsKeyStart-1] == ',':
		return edit{l.annotationsKeyStart - 1, after + 1, ""}, nil
	}
	return edit{}, fmt.Errorf("%w: the annotations of the encrypted layer %s are not those Lajstrom adds", ErrEncryption, l.digest)
}

// edit replaces raw[start:end] with text.
type edit struct {
	start, end int
	text       string
}

// apply returns raw with the edits, which do not overlap, made.
func apply(raw []byte, edits []edit) []byte {
	slices.SortFunc(edits, func(a, b edit) int { return a.start - b.start })
	var out bytes.Buffer
	at := 0
	for _, e := range edits {
		out.Write(raw[at:e.start])
		out.WriteString(e.text)
		at = e.end
	}
	out.Write(raw[at:])
	return out.Bytes()
}

// scanned is where the members of a manifest that the change touches are in its bytes.
type scanned struct {
	layers  []*scannedLayer
	subject *scannedLayer
}

// scannedLayer is where a descriptor's members are in the bytes of its manifest: the raw values
// of mediaType and digest, the opening brace of annotations and the quote of its key (-1 when
// none), and the end of the descriptor, after its closing brace.
type scannedLayer struct {
	mediaType, jwe, pubOpts                   string
	digest                                    digest.Digest
	mediaTypeEnd, digestStart, digestEnd, end int
	annotationsStart, annotationsKeyStart     int
	annotationsEmpty, encAnnotation, data     bool
}

// scanManifest finds the layers and the subject of the manifest raw, a JSON object.
func scanManifest(raw []byte) (*scanned, error) {
	s := &scanner{raw: raw, dec: json.NewDecoder(bytes.NewReader(raw))}
	m := &scanned{}
	if err := s.expect('{'); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for s.dec.More() {
		key, _, err := s.key()
		if err != nil {
			return nil, err
		}
		if seen[key] {
			return nil, fmt.Errorf("%w: the manifest has %q twice", ErrEncryption, key)
		}
		seen[key] = true
		switch key {
		case "layers":
			if m.layers, err = s.layers(); err != nil {
				return nil, err
			}
		case "subject":
			if m.subject, err = s.descriptor(); err != nil {
				return nil, err
			}
		default:
			if err := s.skip(); err != nil {
				return nil, err
			}
		}
	}
	if err := s.expect('}'); err != nil {
		return nil, err
	}
	if _, err := s.dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: more than one JSON value", ErrEncryption)
	}
	return m, nil
}

// scanner reads a manifest's tokens and knows their offsets.
type scanner struct {
	raw []byte
	dec *json.Decoder
}

func (s *scanner) expect(d json.Delim) error {
	t, err := s.dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrEncryption, err)
	}
	if t != d {
		return fmt.Errorf("%w: %v where %v was due", ErrEncryption, t, d)
	}
	return nil
}

// key reads a member's key and returns it with the offset of its opening quote.
func (s *scanner) key() (string, int, error) {
	t, err := s.dec.Token()
	if err != nil {
		return "", 0, fmt.Errorf("%w: %w", ErrEncryption, err)
	}
	k, ok := t.(string)
	if !ok {
		return "", 0, fmt.Errorf("%w: %v where a key was due", ErrEncryption, t)
	}
	end := int(s.dec.InputOffset())
	return k, bytes.LastIndexByte(s.raw[:end-1], '"'), nil
}

// valueStart is the offset of the value after the key that ended at the decoder's offset.
func (s *scanner) valueStart() int {
	i := int(s.dec.InputOffset())
	for i < len(s.raw) && (s.raw[i] == ' ' || s.raw[i] == '\t' || s.raw[i] == '\n' || s.raw[i] == '\r' || s.raw[i] == ':') {
		i++
	}
	return i
}

func (s *scanner) skip() error {
	var v json.RawMessage
	if err := s.dec.Decode(&v); err != nil {
		return fmt.Errorf("%w: %w", ErrEncryption, err)
	}
	return nil
}

// str reads a string value and returns it with the offsets of its raw form.
func (s *scanner) str() (string, int, int, error) {
	start := s.valueStart()
	t, err := s.dec.Token()
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: %w", ErrEncryption, err)
	}
	v, ok := t.(string)
	if !ok {
		return "", 0, 0, fmt.Errorf("%w: %v where a string was due", ErrEncryption, t)
	}
	return v, start, int(s.dec.InputOffset()), nil
}

func (s *scanner) layers() ([]*scannedLayer, error) {
	if err := s.expect('['); err != nil {
		return nil, err
	}
	var out []*scannedLayer
	for s.dec.More() {
		l, err := s.descriptor()
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, s.expect(']')
}

// descriptor reads a descriptor object.
func (s *scanner) descriptor() (*scannedLayer, error) {
	l := &scannedLayer{annotationsStart: -1, annotationsKeyStart: -1}
	if err := s.expect('{'); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for s.dec.More() {
		key, keyStart, err := s.key()
		if err != nil {
			return nil, err
		}
		if seen[key] {
			return nil, fmt.Errorf("%w: a descriptor has %q twice", ErrEncryption, key)
		}
		seen[key] = true
		if err := s.member(l, key, keyStart); err != nil {
			return nil, err
		}
	}
	if err := s.expect('}'); err != nil {
		return nil, err
	}
	l.end = int(s.dec.InputOffset())
	return l, nil
}

// member reads the value of the member key of a descriptor into l.
func (s *scanner) member(l *scannedLayer, key string, keyStart int) error {
	switch key {
	case "mediaType":
		v, _, end, err := s.str()
		l.mediaType, l.mediaTypeEnd = v, end
		return err
	case "digest":
		v, start, end, err := s.str()
		if err != nil {
			return err
		}
		d, perr := digest.Parse(v)
		if raw := s.raw[start:end]; perr != nil || len(raw) != len(v)+2 || string(raw[1:len(raw)-1]) != v {
			return fmt.Errorf("%w: the digest %q is not written as a plain digest", ErrEncryption, v)
		}
		l.digest, l.digestStart, l.digestEnd = d, start, end
		return nil
	case "annotations":
		l.annotationsKeyStart, l.annotationsStart = keyStart, s.valueStart()
		return s.annotations(l)
	case "data":
		l.data = true
	}
	return s.skip()
}

// annotations reads the annotations object of a descriptor.
func (s *scanner) annotations(l *scannedLayer) error {
	if s.byteAt(l.annotationsStart) != '{' {
		return fmt.Errorf("%w: the annotations of a descriptor are not an object", ErrEncryption)
	}
	if err := s.expect('{'); err != nil {
		return err
	}
	l.annotationsEmpty = !s.dec.More()
	for s.dec.More() {
		k, _, err := s.key()
		if err != nil {
			return err
		}
		v, _, _, err := s.str()
		if err != nil {
			return err
		}
		switch {
		case k == AnnotationEncJWE:
			l.jwe, l.encAnnotation = v, true
		case k == AnnotationEncPubOpts:
			l.pubOpts, l.encAnnotation = v, true
		case strings.HasPrefix(k, AnnotationEncPrefix):
			l.encAnnotation = true
		}
	}
	return s.expect('}')
}

// byteAt is the byte of raw at i, 0 past its end.
func (s *scanner) byteAt(i int) byte {
	if i >= len(s.raw) {
		return 0
	}
	return s.raw[i]
}
