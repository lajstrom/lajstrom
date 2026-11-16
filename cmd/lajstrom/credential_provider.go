package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The kubelet's exec plugin protocol for image credentials (KEP-2133, the service-account token
// of KEP-4412): a CredentialProviderRequest on standard input, a CredentialProviderResponse of the
// same version on standard output.
const (
	providerAPIVersion = "credentialprovider.kubelet.k8s.io/v1"
	// providerUser is the user name the plugin answers: Lajstrom reads none for a token of
	// auth.kubernetes (docs/spec/rbac.md §9).
	providerUser = "k8s"
	// providerMargin is how long before the token's exp the kubelet stops reusing the answer.
	providerMargin = time.Minute
	// maxProviderRequest bounds the request the plugin reads.
	maxProviderRequest = 1 << 20
)

// providerRequest is what the plugin reads of a CredentialProviderRequest.
type providerRequest struct {
	APIVersion          string `json:"apiVersion"`
	Kind                string `json:"kind"`
	Image               string `json:"image"`
	ServiceAccountToken string `json:"serviceAccountToken"`
}

// providerAuth is the credential of a registry in a CredentialProviderResponse.
type providerAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// providerResponse is a CredentialProviderResponse.
type providerResponse struct {
	APIVersion    string                  `json:"apiVersion"`
	Kind          string                  `json:"kind"`
	CacheKeyType  string                  `json:"cacheKeyType"`
	CacheDuration string                  `json:"cacheDuration"`
	Auth          map[string]providerAuth `json:"auth"`
}

// newCredentialProviderCmd returns credential-provider, the kubelet's image credential provider
// for Lajstrom (docs/auth.md, Kubernetes service-account tokens): it answers the pod's
// service-account token as the password of the image's registry, which Lajstrom's /v2/token takes
// for a cluster of auth.kubernetes. It reads no configuration and makes no request.
func newCredentialProviderCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "credential-provider",
		Short: "The kubelet's image credential provider: a pod's service-account token as the registry's password (KEP-4412)",
		Long: "Run by the kubelet with a CredentialProviderRequest on standard input; writes the CredentialProviderResponse.\n" +
			"Configure it with tokenAttributes in the kubelet's CredentialProviderConfig: see docs/auth.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return credentialProvider(cmd.InOrStdin(), cmd.OutOrStdout(), time.Now())
		},
	}
}

// credentialProvider reads a CredentialProviderRequest from in and writes its answer to out: the
// service-account token as the password of the image's registry host, under the user name
// providerUser, cached until providerMargin before the token's exp. A request without a token (a
// pod without a service account, when requireServiceAccount is false) gets no credential.
func credentialProvider(in io.Reader, out io.Writer, now time.Time) error {
	var req providerRequest
	if err := json.NewDecoder(io.LimitReader(in, maxProviderRequest)).Decode(&req); err != nil {
		return fmt.Errorf("credential-provider: the request is not JSON: %w", err)
	}
	if req.APIVersion != providerAPIVersion || req.Kind != "CredentialProviderRequest" {
		return fmt.Errorf("credential-provider: a %s of %s, not a CredentialProviderRequest of %s", req.Kind, req.APIVersion, providerAPIVersion)
	}
	resp := providerResponse{APIVersion: providerAPIVersion, Kind: "CredentialProviderResponse", CacheKeyType: "Registry",
		CacheDuration: "0s", Auth: map[string]providerAuth{}}
	if req.ServiceAccountToken != "" {
		host, err := imageHost(req.Image)
		if err != nil {
			return err
		}
		resp.Auth[host] = providerAuth{Username: providerUser, Password: req.ServiceAccountToken}
		resp.CacheDuration = cacheFor(req.ServiceAccountToken, now).String()
	}
	return json.NewEncoder(out).Encode(resp)
}

// imageHost returns the registry host of an image reference, with its port: the part before the
// first "/", which a reference with a registry has.
func imageHost(image string) (string, error) {
	host, _, ok := strings.Cut(image, "/")
	if !ok || host == "" || !strings.ContainsAny(host, ".:") && host != "localhost" {
		return "", fmt.Errorf("credential-provider: the image %q names no registry host", image)
	}
	return host, nil
}

// cacheFor is how long the kubelet may reuse the answer for the token raw: until providerMargin
// before its exp, read without verifying it; 0 when it has none or it is that close.
func cacheFor(raw string, now time.Time) time.Duration {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err != nil || json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return 0
	}
	return max(time.Unix(claims.Exp, 0).Sub(now)-providerMargin, 0).Truncate(time.Second)
}
