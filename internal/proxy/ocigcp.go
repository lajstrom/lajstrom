package proxy

import (
	"context"
	"net/url"
	"strings"

	"github.com/lajstrom/lajstrom/internal/gcpauth"
)

// gcpUser is the Basic username of an access token at Artifact Registry and Container Registry.
const gcpUser = "oauth2accesstoken"

// gcpRegistry reports whether the URL is on a host of Google's registries: gcr.io, its regional
// hosts, or Artifact Registry's *.pkg.dev.
func gcpRegistry(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == "gcr.io" || strings.HasSuffix(host, ".gcr.io") || strings.HasSuffix(host, ".pkg.dev")
}

// gcpBasic returns the generic.Credential.Basic of an oci upstream with auth.gcp
// (docs/spec/proxy-and-group.md §11.1): oauth2accesstoken and the access token of the metadata
// server, from t.
func gcpBasic(t *gcpauth.Tokens) func(context.Context) (string, string, error) {
	return func(ctx context.Context) (string, string, error) {
		token, err := t.Token(ctx)
		if err != nil {
			return "", "", err
		}
		return gcpUser, token, nil
	}
}
