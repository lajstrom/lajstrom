package config

import (
	"fmt"
	"regexp"
	"strings"
)

// The schemes of a key URI (ADR 0020), as cosign writes them.
const (
	// KMSSchemeAWS is AWS KMS: awskms:///<key id | key ARN | alias/<name> | alias ARN>.
	KMSSchemeAWS = "awskms"
	// KMSSchemeGCP is GCP Cloud KMS: gcpkms://projects/<p>/locations/<l>/keyRings/<r>/
	// cryptoKeys/<k>/cryptoKeyVersions/<n>.
	KMSSchemeGCP = "gcpkms"
)

// KMSKey is a key URI taken apart: the KMS and the key's name there.
type KMSKey struct {
	// Scheme is KMSSchemeAWS or KMSSchemeGCP.
	Scheme string
	// Name is what the KMS's API takes as the key: AWS's KeyId (a key id, a key ARN, alias/<name>
	// or an alias ARN), GCP's resource name of a key version.
	Name string
}

// String returns the key URI.
func (k KMSKey) String() string {
	if k.Scheme == KMSSchemeAWS {
		return KMSSchemeAWS + ":///" + k.Name
	}
	return k.Scheme + "://" + k.Name
}

var (
	// gcpKeyVersion is the resource name of a Cloud KMS key version.
	gcpKeyVersion = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+/cryptoKeyVersions/[1-9][0-9]*$`)
	// gcpKey is the resource name of a Cloud KMS key, without its version.
	gcpKey = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+/?$`)
)

// maxKMSName bounds a key's name: AWS's KeyId takes 2048 characters.
const maxKMSName = 2048

// ParseKMSKey parses a key URI: awskms:/// and the KeyId of AWS KMS, or gcpkms:// and the
// resource name of a Cloud KMS key version. A URI with an endpoint (awskms://host/…) is refused:
// AWS_ENDPOINT_URL_KMS sets one. The error says what is wrong, and the URI holds no secret.
func ParseKMSKey(uri string) (KMSKey, error) {
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok {
		return KMSKey{}, fmt.Errorf("%q is not a key URI: awskms:///<key> or gcpkms://projects/…/cryptoKeyVersions/<n>", uri)
	}
	if strings.ContainsFunc(rest, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return KMSKey{}, fmt.Errorf("%q: the key's name has a space, a control character or a character that is not ASCII", uri)
	}
	switch scheme {
	case KMSSchemeAWS:
		name, ok := strings.CutPrefix(rest, "/")
		switch {
		case !ok:
			return KMSKey{}, fmt.Errorf("%q names an endpoint, which a key URI does not take: write awskms:///<key> and set "+
				"AWS_ENDPOINT_URL_KMS", uri)
		case name == "":
			return KMSKey{}, fmt.Errorf("%q names no key: awskms:///<key id | key ARN | alias/<name> | alias ARN>", uri)
		case len(name) > maxKMSName:
			return KMSKey{}, fmt.Errorf("the key's name in %.40q… is longer than %d characters", uri, maxKMSName)
		}
		return KMSKey{Scheme: KMSSchemeAWS, Name: name}, nil
	case KMSSchemeGCP:
		switch {
		case gcpKeyVersion.MatchString(rest):
			return KMSKey{Scheme: KMSSchemeGCP, Name: rest}, nil
		case gcpKey.MatchString(rest):
			return KMSKey{}, fmt.Errorf("%q names a key, not a version of it: add /cryptoKeyVersions/<n>", uri)
		}
		return KMSKey{}, fmt.Errorf("%q is not gcpkms://projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>/cryptoKeyVersions/<n>", uri)
	}
	return KMSKey{}, fmt.Errorf("key URI %q: the scheme is not awskms or gcpkms", uri)
}
