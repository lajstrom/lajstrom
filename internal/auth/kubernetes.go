package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// ErrIssuerUnavailable means the issuer of a Kubernetes cluster did not answer: its discovery
// document or its keys never loaded, so that its tokens can be neither taken nor refused. The
// server answers 503.
var ErrIssuerUnavailable = errors.New("auth: the issuer does not answer")

// maxKubernetesToken bounds a secret that Kubernetes.Recognizes reads as a token: the kubelet's
// tokens are a kilobyte or two.
const maxKubernetesToken = 16 << 10

// TokenVerifier verifies the JWT of an issuer and returns its claims (oidc.Verifier). An issuer
// that does not answer is an error wrapping ErrIssuerUnavailable; any other error is a refusal.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (map[string]any, error)
}

// KubernetesCluster is a cluster of auth.kubernetes: its name, in the principal
// k8s:<name>/<namespace>/<service account>, its issuer, and the verifier of its tokens.
type KubernetesCluster struct {
	Name     string
	Issuer   string
	Verifier TokenVerifier
}

// Kubernetes authenticates the service-account tokens of the clusters of auth.kubernetes, which
// the kubelet's credential provider hands containerd as the password of /v2/token
// (docs/spec/rbac.md §9, KEP-4412). Create it as a struct literal; its methods may be called
// concurrently.
type Kubernetes struct {
	// Clusters are the clusters, each with its own issuer.
	Clusters []KubernetesCluster
	// Log receives the refusals and the principals at debug level; nil means slog.Default().
	Log *slog.Logger
}

// Recognizes reports whether secret is a token of one of the clusters: a JWT, three base64url
// parts, whose iss, read before it is verified, is a cluster's issuer. Any other secret is a
// password.
func (k *Kubernetes) Recognizes(secret string) bool {
	return k.cluster(secret) != nil
}

// cluster returns the cluster whose issuer is the unverified iss of the JWT secret, or nil.
func (k *Kubernetes) cluster(secret string) *KubernetesCluster {
	parts := strings.Split(secret, ".")
	if len(secret) > maxKubernetesToken || len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Issuer == "" {
		return nil
	}
	for i := range k.Clusters {
		if k.Clusters[i].Issuer == claims.Issuer {
			return &k.Clusters[i]
		}
	}
	return nil
}

// Authenticate verifies secret, a token of a cluster (Recognizes), and returns the principal
// k8s:<cluster>/<namespace>/<service account> of its claim kubernetes.io and a CredKubernetes.
// A token that does not verify, or whose sub and kubernetes.io disagree, is ErrBadCredentials,
// logged at info (LogRefusal) with the cluster and its reason at debug, never the token; an
// issuer that does not answer is an error wrapping ErrIssuerUnavailable.
func (k *Kubernetes) Authenticate(ctx context.Context, secret string) (Principal, Credential, error) {
	c := k.cluster(secret)
	if c == nil {
		LogRefusal(ctx, k.log(), string(CredKubernetes), AccountUnknown, 0, "a token of no cluster")
		return Principal{}, Credential{}, ErrBadCredentials
	}
	claims, err := c.Verifier.Verify(ctx, secret)
	if errors.Is(err, ErrIssuerUnavailable) {
		return Principal{}, Credential{}, fmt.Errorf("auth: kubernetes cluster %s: %w", c.Name, err)
	}
	if err == nil {
		var namespace, sa string
		if namespace, sa, err = serviceAccount(claims); err == nil {
			p := Principal{Kind: KindKubernetes, Name: c.Name + "/" + namespace + "/" + sa}
			k.log().DebugContext(ctx, "kubernetes service account", "principal", p.String(), "cluster", c.Name)
			return p, Credential{Kind: CredKubernetes}, nil
		}
	}
	k.log().DebugContext(ctx, "kubernetes token refused", "cluster", c.Name, "reason", err.Error())
	LogRefusal(ctx, k.log(), string(CredKubernetes), AccountUnknown, 0, "a token that does not verify", "cluster", c.Name)
	return Principal{}, Credential{}, ErrBadCredentials
}

// serviceAccount returns the namespace and the service account of the claim kubernetes.io of a
// token, which its sub must name as system:serviceaccount:<namespace>:<name>.
func serviceAccount(claims map[string]any) (namespace, name string, err error) {
	k8s, _ := claims["kubernetes.io"].(map[string]any)
	namespace, _ = k8s["namespace"].(string)
	sa, _ := k8s["serviceaccount"].(map[string]any)
	name, _ = sa["name"].(string)
	if err := CheckKubernetesNames(namespace, name); err != nil {
		return "", "", fmt.Errorf("the claim kubernetes.io: %w", err)
	}
	if sub, _ := claims["sub"].(string); sub != "system:serviceaccount:"+namespace+":"+name {
		return "", "", errors.New("the sub is not the service account of the claim kubernetes.io")
	}
	return namespace, name, nil
}

func (k *Kubernetes) log() *slog.Logger {
	if k.Log == nil {
		return slog.Default()
	}
	return k.Log
}
