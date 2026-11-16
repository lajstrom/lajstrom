package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

var (
	fileDigest  = digest.FromString("the file")
	metaDigest  = digest.FromString("the metadata")
	subjectFrom = digest.FromString("client subject")
	subjectTo   = digest.FromString("stored subject")
)

// encLayers are the ciphertexts of the two plaintext layers.
func encLayers() map[digest.Digest]EncryptedLayer {
	return map[digest.Digest]EncryptedLayer{
		fileDigest: {Stored: digest.FromString("file ciphertext"), JWE: "ZmlsZSBqd2U=", PubOpts: "ZmlsZSBwdWI="},
		metaDigest: {Stored: digest.FromString("meta ciphertext"), JWE: "bWV0YSBqd2U=", PubOpts: "bWV0YSBwdWI="},
	}
}

// plainOf maps the ciphertexts of encLayers back.
func plainOf(d digest.Digest) (digest.Digest, bool) {
	for p, e := range encLayers() {
		if e.Stored == d {
			return p, true
		}
	}
	return "", false
}

// manifests are the forms a client writes: oras-go's compact one, an indented one, a descriptor
// without annotations, a referrer, a media type with an escaped slash, keys in another order.
var manifests = map[string]string{
	"compact": `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.lajstrom.raw.v1",` +
		`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},` +
		`"layers":[{"mediaType":"application/vnd.lajstrom.raw.file.v1","digest":"` + fileDigest.String() + `","size":8,` +
		`"annotations":{"org.opencontainers.image.title":"a/b.txt"}},` +
		`{"mediaType":"application/vnd.lajstrom.raw.metadata.v1+json","digest":"` + metaDigest.String() + `","size":12,` +
		`"annotations":{"org.opencontainers.image.title":"metadata.json"}}],"annotations":{"lajstrom.schema":"1"}}`,
	"indented": "{\n  \"schemaVersion\": 2,\n  \"layers\": [\n    {\n      \"mediaType\": \"application/vnd.lajstrom.raw.file.v1\",\n" +
		"      \"digest\": \"" + fileDigest.String() + "\",\n      \"size\": 8,\n      \"annotations\": {\n" +
		"        \"org.opencontainers.image.title\": \"a/b.txt\"\n      }\n    },\n    {\n" +
		"      \"digest\": \"" + metaDigest.String() + "\",\n      \"mediaType\": \"x+json\",\n      \"size\": 12\n    }\n  ]\n}\n",
	"no annotations": `{"schemaVersion":2,"layers":[{"mediaType":"application/vnd.dev.cosign.simplesigning.v1+json","digest":"` + fileDigest.String() +
		`","size":8}]}`,
	"referrer": `{"schemaVersion":2,"layers":[{"mediaType":"application/spdx+json","size":8,"digest":"` + fileDigest.String() + `"}],` +
		`"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + subjectFrom.String() + `","size":500}}`,
	"escaped slash": `{"layers":[{"mediaType":"application\/vnd.lajstrom.raw.file.v1","digest":"` + fileDigest.String() + `","size":8,` +
		`"annotations":{"a":"b","c":"d"}}]}`,
}

// TestEncryptManifest: the change of every form undoes to the client's bytes exactly, and the
// stored manifest is the encrypted one ocicrypt reads: the ciphertext digests, +encrypted media
// types, the two annotations, the subject's stored digest.
func TestEncryptManifest(t *testing.T) {
	for name, raw := range manifests {
		t.Run(name, func(t *testing.T) {
			subject := digest.Digest("")
			if strings.Contains(raw, `"subject"`) {
				subject = subjectTo
			}
			stored, err := EncryptManifest([]byte(raw), encLayers(), subject)
			if err != nil {
				t.Fatal(err)
			}
			checkStored(t, stored, subject)
			back, err := DecryptManifest(stored, plainOf, map[bool]digest.Digest{true: subjectFrom}[subject != ""])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(back, []byte(raw)) {
				t.Errorf("undone:\n%s\nwant the client's:\n%s", back, raw)
			}
		})
	}
}

// checkStored checks the stored manifest: JSON, the ciphertext digests, +encrypted media types,
// the two annotations, the subject's stored digest, as EncryptedLayers finds them too.
func checkStored(t *testing.T, stored []byte, subject digest.Digest) {
	t.Helper()
	var m ocispec.Manifest
	if err := json.Unmarshal(stored, &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stored)
	}
	for _, l := range m.Layers {
		p, ok := plainOf(l.Digest)
		want := encLayers()[p]
		if !ok || !strings.HasSuffix(l.MediaType, EncryptedSuffix) || l.Annotations[AnnotationEncJWE] != want.JWE ||
			l.Annotations[AnnotationEncPubOpts] != want.PubOpts {
			t.Errorf("a stored layer %+v", l)
		}
	}
	wantSub := digest.Digest("")
	if m.Subject != nil {
		wantSub = m.Subject.Digest
	}
	if subject != "" && wantSub != subjectTo {
		t.Errorf("the stored subject %+v", m.Subject)
	}
	found, sub, err := EncryptedLayers(stored)
	if err != nil || len(found) != len(m.Layers) || sub != wantSub {
		t.Errorf("EncryptedLayers = %+v, %s, %v", found, sub, err)
	}
}

// TestEncryptManifestRefuses: what the change could not undo exactly, a layer encrypted already,
// one without its ciphertext, one with its bytes in data.
func TestEncryptManifestRefuses(t *testing.T) {
	layer := func(extra string) string {
		return `{"layers":[{"mediaType":"x","digest":"` + fileDigest.String() + `","size":8` + extra + `}]}`
	}
	for name, raw := range map[string]string{
		"a member twice":      layer(`,"size":9`),
		"an escaped digest":   `{"layers":[{"mediaType":"x","digest":"sha256\u003a` + fileDigest.Encoded() + `","size":8}]}`,
		"empty annotations":   layer(`,"annotations":{}`),
		"null annotations":    layer(`,"annotations":null`),
		"data":                layer(`,"data":"dGhlIGZpbGU="`),
		"encrypted already":   `{"layers":[{"mediaType":"x+encrypted","digest":"` + fileDigest.String() + `","size":8}]}`,
		"an enc annotation":   layer(`,"annotations":{"org.opencontainers.image.enc.keys.pgp":"x"}`),
		"no ciphertext":       `{"layers":[{"mediaType":"x","digest":"` + digest.FromString("other").String() + `","size":8}]}`,
		"no digest":           `{"layers":[{"mediaType":"x","size":8}]}`,
		"layers twice":        `{"layers":[],"layers":[]}`,
		"not an object":       `[]`,
		"two values":          layer("") + "{}",
		"a truncated payload": `{"layers":[{"mediaType":"x"`,
	} {
		if _, err := EncryptManifest([]byte(raw), encLayers(), ""); !errors.Is(err, ErrEncryption) || !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrEncryption", name, err)
		}
	}
}

// TestDecryptManifestRefuses: a stored manifest whose encryption is not the change of
// EncryptManifest, or whose plaintext digests are not known, is not answered.
func TestDecryptManifestRefuses(t *testing.T) {
	stored, err := EncryptManifest([]byte(manifests["compact"]), encLayers(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecryptManifest(stored, func(digest.Digest) (digest.Digest, bool) { return "", false }, ""); !errors.Is(err, ErrEncryption) {
		t.Errorf("unknown plaintext: %v", err)
	}
	moved := bytes.Replace(stored, []byte(`{"org.opencontainers.image.enc.keys.jwe"`), []byte(`{"z":"y","org.opencontainers.image.enc.keys.jwe"`), 1)
	if _, err := DecryptManifest(moved, plainOf, ""); !errors.Is(err, ErrEncryption) {
		t.Errorf("annotations moved: %v", err)
	}
	plain := []byte(manifests["compact"])
	if back, err := DecryptManifest(plain, plainOf, ""); err != nil || !bytes.Equal(back, plain) {
		t.Errorf("a manifest without encrypted layers: %v, changed %v", err, !bytes.Equal(back, plain))
	}
}

func TestCheckPlainLayers(t *testing.T) {
	ok := []LayerInfo{{MediaType: "application/vnd.lajstrom.raw.file.v1", Annotations: map[string]string{"org.opencontainers.image.title": "x"}}}
	if err := CheckPlainLayers(ok); err != nil {
		t.Error(err)
	}
	for _, bad := range [][]LayerInfo{
		{{MediaType: "application/vnd.lajstrom.raw.file.v1+encrypted"}},
		{{MediaType: "x", Annotations: map[string]string{"org.opencontainers.image.enc.pubopts": "x"}}},
	} {
		if err := CheckPlainLayers(bad); !errors.Is(err, ErrEncryption) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// TestEncryptManifestShapes: descriptors in any order of members, with any whitespace, with and
// without annotations, undo to their bytes exactly.
func TestEncryptManifestShapes(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // shapes of a test, the same on every run
	ws := func() string { return []string{"", " ", "\n", "\n    ", "\t"}[r.IntN(5)] }
	for i := range 500 {
		var b strings.Builder
		b.WriteString("{" + ws() + `"layers"` + ws() + ":" + ws() + "[")
		layers := []digest.Digest{fileDigest, metaDigest}[:1+r.IntN(2)]
		for j, d := range layers {
			if j > 0 {
				b.WriteString("," + ws())
			}
			members := []string{
				ws() + `"mediaType"` + ws() + ":" + ws() + `"application/x"` + ws(),
				ws() + `"digest"` + ws() + ":" + ws() + `"` + d.String() + `"` + ws(),
				ws() + `"size"` + ws() + ":" + ws() + "8" + ws(),
			}
			if r.IntN(2) == 0 {
				members = append(members, ws()+`"annotations"`+ws()+":"+ws()+"{"+ws()+`"k"`+ws()+":"+ws()+`"v"`+ws()+"}"+ws())
			}
			r.Shuffle(len(members), func(a, b int) { members[a], members[b] = members[b], members[a] })
			b.WriteString("{" + strings.Join(members, ",") + "}")
		}
		b.WriteString(ws() + "]" + ws() + "}" + ws())
		raw := []byte(b.String())
		stored, err := EncryptManifest(raw, encLayers(), "")
		if err != nil {
			t.Fatalf("shape %d %s: %v", i, raw, err)
		}
		if !json.Valid(stored) {
			t.Fatalf("shape %d: not JSON:\n%s", i, stored)
		}
		back, err := DecryptManifest(stored, plainOf, "")
		if err != nil || !bytes.Equal(back, raw) {
			t.Fatalf("shape %d: %v\n got %q\nwant %q", i, err, back, raw)
		}
	}
}
