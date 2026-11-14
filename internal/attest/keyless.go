package attest

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	sgbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	sgverify "github.com/sigstore/sigstore-go/pkg/verify"
)

// Identity is one entry of attachments.keyless.identities: a signer of keyless bundles, as the
// Fulcio certificate names it (docs/spec/manifest-profile.md §6.3).
type Identity struct {
	// Name is the signedBy of what it verifies; unique among the keys and identities of a trust.
	Name string
	// Issuer is the OIDC issuer the certificate must name, exactly:
	// https://token.actions.githubusercontent.com.
	Issuer string
	// Subject is the subject alternative name the certificate must have, exactly (a URI or an
	// email address); SubjectRegex a Go regular expression it must match instead. One of the two.
	Subject, SubjectRegex string
	// PredicateTypes are the predicate types the identity is trusted for; empty: every one,
	// signatures included (they count as PredicateCosignSign).
	PredicateTypes []string
}

// Keyless is attachments.keyless: the offline trusted root keyless bundles verify against, and
// the identities trusted to sign them.
type Keyless struct {
	// TrustedRoot is a trusted_root.json, sigstore's TrustedRoot of media type
	// application/vnd.dev.sigstore.trustedroot+json;version=0.1.
	TrustedRoot []byte
	// Identities are the trusted signers.
	Identities []Identity
	// SignedCertificateTimestamps is how many signed certificate timestamps of a CT log of the
	// trusted root a certificate must carry; 0 for a Fulcio without a CT log.
	SignedCertificateTimestamps int
}

// Option configures a Trust.
type Option func(*Trust) error

// WithKeyless makes the trust verify keyless bundles (manifest-profile.md §6.3): against the
// trusted root of k, offline, signed by one of its identities. A trusted root that does not parse,
// an identity without a name, issuer or subject, a name the trust has already, or a regular
// expression that does not compile is an error.
func WithKeyless(k Keyless) Option {
	return func(t *Trust) error {
		tr, err := root.NewTrustedRootFromJSON(k.TrustedRoot)
		if err != nil {
			return fmt.Errorf("the trusted root: %w", err)
		}
		if k.SignedCertificateTimestamps < 0 {
			return errors.New("the number of signed certificate timestamps is negative")
		}
		opts := []sgverify.VerifierOption{sgverify.WithTransparencyLog(1), sgverify.WithObserverTimestamps(1), sgverify.WithoutStatementPredicate()}
		if k.SignedCertificateTimestamps > 0 {
			opts = append(opts, sgverify.WithSignedCertificateTimestamps(k.SignedCertificateTimestamps))
		}
		v, err := sgverify.NewVerifier(tr, opts...)
		if err != nil {
			return fmt.Errorf("the keyless verifier: %w", err)
		}
		kt := &keylessTrust{verifier: v}
		names := map[string]bool{}
		for _, k := range t.keys {
			names[k.name] = true
		}
		for i, id := range k.Identities {
			switch {
			case id.Name == "":
				return fmt.Errorf("keyless identity %d has no name", i+1)
			case names[id.Name]:
				return fmt.Errorf("the trust names %q twice", id.Name)
			case id.Issuer == "":
				return fmt.Errorf("the keyless identity %q has no issuer", id.Name)
			case (id.Subject == "") == (id.SubjectRegex == ""):
				return fmt.Errorf("the keyless identity %q needs one of a subject and a subject regex", id.Name)
			}
			names[id.Name] = true
			match, err := sgverify.NewShortCertificateIdentity(id.Issuer, "", id.Subject, id.SubjectRegex)
			if err != nil {
				return fmt.Errorf("the keyless identity %q: %w", id.Name, err)
			}
			kt.identities = append(kt.identities, keylessIdentity{name: id.Name, match: match, types: typeSet(id.PredicateTypes)})
		}
		t.keyless = kt
		t.keylessHash = keylessHash(k)
		return nil
	}
}

// typeSet is the set of the predicate types, nil for every one.
func typeSet(types []string) map[string]bool {
	if len(types) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, p := range types {
		set[p] = true
	}
	return set
}

// keylessHash is what Hash takes of k: its identities in name order, the sha256 of its trusted
// root and its SCT threshold.
func keylessHash(k Keyless) string {
	ids := slices.Clone(k.Identities)
	slices.SortFunc(ids, func(a, b Identity) int { return strings.Compare(a.Name, b.Name) })
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "keyless\x00%x\x00%d\n", sha256.Sum256(k.TrustedRoot), k.SignedCertificateTimestamps)
	for _, id := range ids {
		types := slices.Clone(id.PredicateTypes)
		slices.Sort(types)
		_, _ = fmt.Fprintf(&b, "%s\x00%s\x00%s\x00%s\x00%s\n", strconv.Quote(id.Name), strconv.Quote(id.Issuer), strconv.Quote(id.Subject),
			strconv.Quote(id.SubjectRegex), strconv.Quote(strings.Join(slices.Compact(types), "\x00")))
	}
	return b.String()
}

// keylessTrust verifies keyless bundles: sigstore-go's verifier over the trusted root, and the
// identities.
type keylessTrust struct {
	verifier   *sgverify.Verifier
	identities []keylessIdentity
}

type keylessIdentity struct {
	name  string
	match sgverify.CertificateIdentity
	types map[string]bool // nil: every predicate type
}

// keyless labels r, the result of an envelope that came with a certificate and that no trusted
// key verified. bundleJSON is the sigstore bundle, nil for cosign's legacy layer, which
// sigstore-go does not read. Without attachments.keyless, or for a legacy layer, r stays
// unsupported. Otherwise sigstore-go verifies the bundle against the trusted root, offline: the
// certificate's chain and validity at the signing time, the transparency log entry, the
// timestamps, the SCTs and the envelope's signature; then the certificate's identity must be
// one of the trusted identities, trusted for the predicate type. The statement's subject is the
// caller's to check, as for a key.
func (t *Trust) keylessResult(r Result, bundleJSON []byte) Result {
	switch {
	case t == nil || t.keyless == nil:
		return r.with(Unsupported, "keyless signatures are not verified: attachments.keyless is not configured")
	case bundleJSON == nil:
		return r.with(Unsupported, "cosign's legacy keyless signatures are not verified; cosign v3 writes a sigstore bundle, which is")
	}
	var b sgbundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return r.with(Invalid, "the sigstore bundle does not parse as sigstore-go reads it: "+err.Error())
	}
	// The artifact is the statement's subject, which the caller checks against the version, and
	// the identity is matched below, for each predicate type its own.
	res, err := t.keyless.verifier.Verify(&b, sgverify.NewPolicy(sgverify.WithoutArtifactUnsafe(), sgverify.WithoutIdentitiesUnsafe()))
	if err != nil {
		return r.with(Untrusted, "the keyless signature does not verify against the trusted root: "+err.Error())
	}
	if res.Signature == nil || res.Signature.Certificate == nil {
		return r.with(Untrusted, "the keyless signature names no certificate identity")
	}
	cert := *res.Signature.Certificate
	other := ""
	for _, id := range t.keyless.identities {
		if id.match.Verify(cert) != nil {
			continue
		}
		if id.types == nil || id.types[r.PredicateType] {
			r.Verification, r.SignedBy, r.Reason = Verified, id.name, ""
			return r
		}
		if other == "" {
			other = id.name
		}
	}
	if other != "" {
		return r.with(Untrusted, fmt.Sprintf("the identity %s signed it, and is not trusted for the predicate type %q", other, r.PredicateType))
	}
	return r.with(Untrusted, fmt.Sprintf("the certificate's identity, issuer %q and subject %q, is not a trusted identity", cert.Issuer, cert.SubjectAlternativeName))
}
