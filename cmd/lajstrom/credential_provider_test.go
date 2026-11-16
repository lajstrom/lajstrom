package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCredentialProvider: the token of the request is the password of the image's registry,
// cached until a minute before its exp; a request without a token gets no credential; another
// version or kind is refused.
func TestCredentialProvider(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	enc := base64.RawURLEncoding.EncodeToString
	token := enc([]byte(`{"alg":"RS256"}`)) + "." + enc(fmt.Appendf(nil, `{"exp":%d}`, now.Add(10*time.Minute).Unix())) + ".sig"
	for _, tt := range []struct {
		name, request, want string
	}{
		{"a token", `{"apiVersion":"credentialprovider.kubelet.k8s.io/v1","kind":"CredentialProviderRequest",` +
			`"image":"lajstrom.example.com:8443/oci/k8s/pause:3.10","serviceAccountToken":"` + token + `","serviceAccountAnnotations":{}}`,
			`{"apiVersion":"credentialprovider.kubelet.k8s.io/v1","kind":"CredentialProviderResponse","cacheKeyType":"Registry",` +
				`"cacheDuration":"9m0s","auth":{"lajstrom.example.com:8443":{"username":"k8s","password":"` + token + `"}}}`},
		{"a token without exp", `{"apiVersion":"credentialprovider.kubelet.k8s.io/v1","kind":"CredentialProviderRequest",` +
			`"image":"localhost/oci/k8s/pause:3.10","serviceAccountToken":"a.b.c"}`,
			`{"apiVersion":"credentialprovider.kubelet.k8s.io/v1","kind":"CredentialProviderResponse","cacheKeyType":"Registry",` +
				`"cacheDuration":"0s","auth":{"localhost":{"username":"k8s","password":"a.b.c"}}}`},
		{"no token", `{"apiVersion":"credentialprovider.kubelet.k8s.io/v1","kind":"CredentialProviderRequest","image":"lajstrom.example.com/x"}`,
			`{"apiVersion":"credentialprovider.kubelet.k8s.io/v1","kind":"CredentialProviderResponse","cacheKeyType":"Registry",` +
				`"cacheDuration":"0s","auth":{}}`},
	} {
		var out bytes.Buffer
		if err := credentialProvider(strings.NewReader(tt.request), &out, now); err != nil || strings.TrimSpace(out.String()) != tt.want {
			t.Errorf("%s: %v\n%s\nwant\n%s", tt.name, err, out.String(), tt.want)
		}
	}
	for _, request := range []string{
		"not JSON",
		`{"apiVersion":"credentialprovider.kubelet.k8s.io/v1beta1","kind":"CredentialProviderRequest","image":"a.example/x"}`,
		`{"apiVersion":"credentialprovider.kubelet.k8s.io/v1","kind":"Other","image":"a.example/x"}`,
		`{"apiVersion":"credentialprovider.kubelet.k8s.io/v1","kind":"CredentialProviderRequest","image":"pause:3.10","serviceAccountToken":"a.b.c"}`,
	} {
		var out bytes.Buffer
		if err := credentialProvider(strings.NewReader(request), &out, now); err == nil || out.Len() != 0 {
			t.Errorf("%s: %v, %q", request, err, out.String())
		}
	}
}
