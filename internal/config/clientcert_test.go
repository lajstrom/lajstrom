package config

import (
	"strings"
	"testing"
)

// TestClientCertificates: auth.clientCertificates (docs/spec/rbac.md §9) loads as written, and
// each rule of its validation names its key.
func TestClientCertificates(t *testing.T) {
	c, err := Load(writeConfig(t, minimal+`  trustedProxies: ["10.0.0.0/8"]
auth:
  clientCertificates:
    forwardedHeader: X-SSL-Client-Cert
    bundles:
      - {name: cluster-a, caFile: /ca/a.pem, trustDomain: cluster-a.example}
      - {name: corp, caFile: /ca/corp.pem, identity: cn}
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Auth.ClientCertificates; got.ForwardedHeader != "X-SSL-Client-Cert" || len(got.Bundles) != 2 ||
		got.Bundles[0] != (CertBundle{Name: "cluster-a", CAFile: "/ca/a.pem", TrustDomain: "cluster-a.example"}) ||
		got.Bundles[1] != (CertBundle{Name: "corp", CAFile: "/ca/corp.pem", Identity: CertIdentityCN}) {
		t.Errorf("auth.clientCertificates = %+v", got)
	}
	for _, tt := range []struct {
		name, file string
		want       []string
	}{
		{"names and files", `auth: {clientCertificates: {bundles: [
  {name: Corp, caFile: /a, identity: cn},
  {name: spiffe, caFile: /a, identity: cn},
  {name: a, caFile: /a, identity: dns},
  {name: a, identity: dns}]}}
`, []string{`bundles[0].name: "Corp" is not lowercase`, `bundles[1].name: "spiffe" is reserved`,
			`bundles[3].name: "a" names another bundle too`, "bundles[3].caFile: must be set"}},
		{"identities", `auth: {clientCertificates: {bundles: [
  {name: a, caFile: /a},
  {name: b, caFile: /a, trustDomain: "Cluster A"},
  {name: c, caFile: /a, trustDomain: td},
  {name: d, caFile: /a, identity: spiffe, trustDomain: td},
  {name: e, caFile: /a, identity: cn, trustDomain: td},
  {name: f, caFile: /a, identity: san}]}}
`, []string{"bundles[0].trustDomain: must be a SPIFFE trust domain", "bundles[1].trustDomain: must be a SPIFFE trust domain",
			`bundles[3].trustDomain: "td" is another bundle's`, "bundles[4].trustDomain: is for a spiffe bundle only",
			`bundles[5].identity: "san" is not spiffe, cn or dns`}},
		{"a header without a bundle", "  trustedProxies: [10.0.0.1]\nauth: {clientCertificates: {forwardedHeader: X-Client-Cert}}\n",
			[]string{"auth.clientCertificates.forwardedHeader: needs a bundle"}},
		{"a header without trusted proxies", "auth: {clientCertificates: {forwardedHeader: X-Client-Cert, bundles: [{name: a, caFile: /a, identity: cn}]}}\n",
			[]string{"auth.clientCertificates.forwardedHeader: is read from server.trustedProxies only"}},
		{"not a header name", "  trustedProxies: [10.0.0.1]\nauth: {clientCertificates: {forwardedHeader: \"X Client\", bundles: [{name: a, caFile: /a, identity: cn}]}}\n",
			[]string{`auth.clientCertificates.forwardedHeader: "X Client" is not a header name`}},
	} {
		_, err := Load(writeConfig(t, minimal+tt.file), nil)
		for _, w := range tt.want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("%s: Load = %v; want %q", tt.name, err, w)
			}
		}
	}
}
