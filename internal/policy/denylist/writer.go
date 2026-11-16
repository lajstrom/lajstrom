package denylist

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Signer signs the document: the policy key (keys.PolicyKey).
type Signer interface {
	Sign(ctx context.Context, msg []byte) ([]byte, error)
	Fingerprint() string
}

// Writer writes the deny list into the registry (docs/spec/manifest-profile.md §10) when the
// policy generation moves, at most once every Every.
type Writer struct {
	// Index is the index database; required.
	Index index.Store
	// Provider is the registry policy.denylist.registry names; required.
	Provider provider.Provider
	// Key signs the document; required.
	Key Signer
	// Locate gives a version's own download path (format.Locator); nil gives none.
	Locate Locate
	// Every is how often the generation is read; zero means 10 seconds.
	Every time.Duration
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
}

// DefaultEvery is Writer.Every when it is zero.
const DefaultEvery = 10 * time.Second

// Run writes the deny list whenever the policy generation is not the one the greatest tag of the
// repository carries, and reads the generation every Every, until ctx is done. A failure is
// logged, and the next turn tries again.
func (w *Writer) Run(ctx context.Context) {
	log := cmp.Or(w.Log, slog.Default())
	t := time.NewTicker(cmp.Or(w.Every, DefaultEvery))
	defer t.Stop()
	for {
		if tag, err := w.Sync(ctx); err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "policy: the deny list is not written; it is tried again", "repository", Repository, "error", err.Error())
		} else if tag != "" {
			log.InfoContext(ctx, "policy: the deny list written", "repository", Repository, "tag", tag)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sync writes the deny list when the greatest tag of the repository carries another generation
// than the index's, and returns the tag it wrote, "" when it wrote none.
func (w *Writer) Sync(ctx context.Context) (string, error) {
	gen, err := w.Index.PolicyGeneration(ctx)
	if err != nil {
		return "", err
	}
	if have, err := w.written(ctx); err != nil || have == gen {
		return "", err
	}
	return w.Write(ctx)
}

// written returns the generation of the greatest tag of the repository, -1 without one.
func (w *Writer) written(ctx context.Context) (int64, error) {
	rr, err := w.Provider.Repository(ctx, Repository)
	if err != nil {
		return 0, err
	}
	var tags []string
	err = rr.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	})
	if errors.Is(err, provider.ErrNotFound) {
		return -1, nil
	} else if err != nil {
		return 0, err
	}
	gen, ok := GenerationOf(Greatest(tags))
	if !ok {
		return -1, nil
	}
	return gen, nil
}

// GenerationOf returns the generation that a tag of the deny list carries
// (<YYYYMMDDTHHMMSSZ>-<generation, 12 digits>-<12 hex>); false for another tag.
func GenerationOf(tag string) (int64, bool) {
	parts := strings.Split(tag, "-")
	if len(parts) != 3 || len(parts[0]) != len("20060102T150405Z") || len(parts[1]) != 12 || len(parts[2]) != 12 {
		return 0, false
	}
	if _, err := time.Parse("20060102T150405Z", parts[0]); err != nil {
		return 0, false
	}
	gen, err := strconv.ParseInt(parts[1], 10, 64)
	return gen, err == nil
}

// Write builds the deny list, signs it and pushes it under its tag, and returns the tag.
func (w *Writer) Write(ctx context.Context) (string, error) {
	doc, err := Build(ctx, w.Index, nil, w.Locate)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	sig, err := w.Key.Sign(ctx, body)
	if err != nil {
		return "", fmt.Errorf("denylist: sign: %w", err)
	}
	sigBytes := []byte(base64.StdEncoding.EncodeToString(sig))
	docDesc := ocispec.Descriptor{MediaType: DocumentMediaType, Digest: digest.FromBytes(body), Size: int64(len(body)),
		Annotations: map[string]string{ocispec.AnnotationTitle: DocumentTitle}}
	sigDesc := ocispec.Descriptor{MediaType: SignatureType, Digest: digest.FromBytes(sigBytes), Size: int64(len(sigBytes)),
		Annotations: map[string]string{ocispec.AnnotationTitle: SignatureTitle}}
	m := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: ArtifactType, Config: ocispec.DescriptorEmptyJSON, Layers: []ocispec.Descriptor{docDesc, sigDesc},
		Annotations: map[string]string{"lajstrom.schema": "1", "lajstrom.policy.generation": strconv.FormatInt(doc.Generation, 10),
			"lajstrom.policy.key": w.Key.Fingerprint(), ocispec.AnnotationCreated: doc.CreatedAt.UTC().Format(time.RFC3339)}}
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	d := digest.FromBytes(raw)
	tag := fmt.Sprintf("%s-%012d-%s", doc.CreatedAt.UTC().Format("20060102T150405Z"), doc.Generation, d.Encoded()[:12])
	if err := w.Provider.EnsureRepository(ctx, Repository); err != nil {
		return "", fmt.Errorf("denylist: make %s: %w", Repository, err)
	}
	rr, err := w.Provider.Repository(ctx, Repository)
	if err != nil {
		return "", err
	}
	blobs := []struct {
		desc ocispec.Descriptor
		data []byte
	}{{ocispec.DescriptorEmptyJSON, ocispec.DescriptorEmptyJSON.Data}, {docDesc, body}, {sigDesc, sigBytes}}
	for _, b := range blobs {
		ok, err := rr.Exists(ctx, b.desc)
		if err == nil && !ok {
			err = rr.Push(ctx, b.desc, bytes.NewReader(b.data))
		}
		if err != nil {
			return "", fmt.Errorf("denylist: push %s to %s: %w", b.desc.Digest, Repository, err)
		}
	}
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(raw))}
	if err := rr.PushReference(ctx, desc, bytes.NewReader(raw), tag); err != nil {
		return "", fmt.Errorf("denylist: push %s:%s: %w", Repository, tag, err)
	}
	return tag, nil
}

// Greatest returns the greatest tag of the deny list among tags, "" when none is one.
func Greatest(tags []string) string {
	var out []string
	for _, t := range tags {
		if _, ok := GenerationOf(t); ok {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return slices.Max(out)
}
