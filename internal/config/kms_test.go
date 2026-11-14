package config

import (
	"strings"
	"testing"
)

// TestParseKMSKey takes the key URIs of ADR 0020 apart and refuses the others with an error
// that says why; String gives the URI back.
func TestParseKMSKey(t *testing.T) {
	const version = "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/3"
	for _, tt := range []struct {
		uri, scheme, name, err string
	}{
		{uri: "awskms:///1234abcd-12ab-34cd-56ef-1234567890ab", scheme: KMSSchemeAWS, name: "1234abcd-12ab-34cd-56ef-1234567890ab"},
		{uri: "awskms:///arn:aws:kms:eu-west-1:111122223333:key/1234abcd", scheme: KMSSchemeAWS,
			name: "arn:aws:kms:eu-west-1:111122223333:key/1234abcd"},
		{uri: "awskms:///alias/lajstrom-apt", scheme: KMSSchemeAWS, name: "alias/lajstrom-apt"},
		{uri: "awskms:///arn:aws-cn:kms:cn-north-1:111122223333:alias/apt", scheme: KMSSchemeAWS,
			name: "arn:aws-cn:kms:cn-north-1:111122223333:alias/apt"},
		{uri: "gcpkms://" + version, scheme: KMSSchemeGCP, name: version},
		{uri: "awskms://localhost:4566/alias/x", err: "names an endpoint"},
		{uri: "awskms:///", err: "names no key"},
		{uri: "awskms:///" + strings.Repeat("k", 2049), err: "longer than 2048"},
		{uri: "awskms:///alias/a b", err: "a space"},
		{uri: "gcpkms://projects/p/locations/global/keyRings/r/cryptoKeys/k", err: "not a version of it"},
		{uri: "gcpkms://projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/0", err: "is not gcpkms://"},
		{uri: "gcpkms://keyRings/r", err: "is not gcpkms://"},
		{uri: "azurekms://vault/key", err: "not awskms or gcpkms"},
		{uri: "alias/lajstrom-apt", err: "is not a key URI"},
	} {
		t.Run(tt.uri[:min(len(tt.uri), 40)], func(t *testing.T) {
			k, err := ParseKMSKey(tt.uri)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("ParseKMSKey(%q) = %+v, %v; want an error with %q", tt.uri, k, err, tt.err)
				}
				return
			}
			if err != nil || k.Scheme != tt.scheme || k.Name != tt.name {
				t.Fatalf("ParseKMSKey(%q) = %+v, %v; want %s %s", tt.uri, k, err, tt.scheme, tt.name)
			}
			if k.String() != tt.uri {
				t.Errorf("String() = %q, want %q", k.String(), tt.uri)
			}
		})
	}
}
