package config

import (
	"strings"
	"testing"
)

// TestKubernetesClusters: auth.kubernetes (docs/spec/rbac.md §9) loads as written, the audience
// defaults to server.externalURL, and each rule of its validation names its key.
func TestKubernetesClusters(t *testing.T) {
	c, err := Load(writeConfig(t, minimal+`auth:
  kubernetes:
    clusters:
      - {name: prod, issuer: "https://oidc.eks.example/id/ABC"}
      - {name: talos, issuer: "https://10.5.0.2:6443", audience: lajstrom, caFile: /ca/talos.pem}
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := c.Auth.Kubernetes.Clusters
	if len(got) != 2 || got[0] != (KubernetesCluster{Name: "prod", Issuer: "https://oidc.eks.example/id/ABC"}) ||
		got[1] != (KubernetesCluster{Name: "talos", Issuer: "https://10.5.0.2:6443", Audience: "lajstrom", CAFile: "/ca/talos.pem"}) {
		t.Errorf("auth.kubernetes.clusters = %+v", got)
	}
	if a := got[0].AudienceOr("https://art.example.com/"); a != "https://art.example.com" {
		t.Errorf("the default audience is %q", a)
	}
	_, err = Load(writeConfig(t, minimal+`auth: {kubernetes: {clusters: [
  {name: Prod, issuer: "https://a.example"},
  {name: spiffe, issuer: "http://b.example"},
  {name: c, issuer: "https://a.example"},
  {name: c, issuer: "https://d.example/?x=1"},
  {name: e, issuer: ""}]}}
`), nil)
	for _, w := range []string{`clusters[0].name: "Prod" is not lowercase`, `clusters[1].name: "spiffe" is reserved`,
		"clusters[1].issuer: must be https", `clusters[2].issuer: "https://a.example" is another cluster's issuer`,
		`clusters[3].name: "c" names another cluster too`, "clusters[3].issuer: must be an absolute http(s) URL without a query",
		"clusters[4].issuer: must be an absolute"} {
		if err == nil || !strings.Contains(err.Error(), "auth.kubernetes."+w) {
			t.Errorf("Load = %v; want %q", err, w)
		}
	}
	_, err = Load(writeConfig(t, listenOnly+"  insecureHTTP: true\nauth: {kubernetes: {clusters: [{name: a, issuer: \"http://a.example\"}]}}\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "auth.kubernetes.clusters[0].audience: must be set when server.externalURL is empty") ||
		strings.Contains(err.Error(), "clusters[0].issuer") {
		t.Errorf("Load without externalURL = %v", err)
	}
}
