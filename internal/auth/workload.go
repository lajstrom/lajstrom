package auth

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/lajstrom/lajstrom/internal/config"
)

// The principals of node and workload identity (docs/spec/rbac.md §9): cert:spiffe://<trust
// domain>/<path> and cert:<bundle>/<name> of a client certificate, k8s:<cluster>/<namespace>/
// <service account> of a Kubernetes service-account token. A binding's principal of these kinds
// may be a pattern, whose last segment is "*"; the principal of a request never is.

// SPIFFEScheme begins the name of the cert: principal of a SPIFFE ID.
const SPIFFEScheme = "spiffe://"

// Bounds of the names of node and workload principals, in bytes.
const (
	// MaxCertificateName bounds the CN or DNS name of a certificate of a cn or dns bundle.
	MaxCertificateName = 255
	// maxSPIFFEID bounds a SPIFFE ID (SPIFFE ID §2.3).
	maxSPIFFEID = 2048
	// maxNamespace and maxServiceAccount bound a Kubernetes namespace (a DNS label) and the name
	// of a service account (a DNS subdomain).
	maxNamespace      = 63
	maxServiceAccount = 253
)

var (
	trustDomainPattern   = regexp.MustCompile(`^[a-z0-9._-]+$`)
	spiffeSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	dnsLabelPattern      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsSubdomainPattern  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// IsPattern reports whether p is a pattern of docs/spec/rbac.md §9: a cert: or k8s: principal
// whose last segment is "*". Only a binding has one.
func (p Principal) IsPattern() bool {
	return (p.Kind == KindCert || p.Kind == KindKubernetes) && strings.HasSuffix(p.Name, "/*")
}

// Matches reports whether the pattern p matches q, a principal of its kind that begins with p's
// name without its "*" and goes on past it. A principal that is not a pattern matches nothing.
func (p Principal) Matches(q Principal) bool {
	if !p.IsPattern() || q.Kind != p.Kind || q.IsPattern() {
		return false
	}
	prefix := strings.TrimSuffix(p.Name, "*")
	return len(q.Name) > len(prefix) && strings.HasPrefix(q.Name, prefix)
}

// CheckCertificateName returns nil when name may be the CN or DNS name of a certificate in the
// principal cert:<bundle>/<name>: 1 to MaxCertificateName bytes of printable ASCII without a
// space and without "*".
func CheckCertificateName(name string) error {
	if name == "" || len(name) > MaxCertificateName {
		return fmt.Errorf("a certificate's name is 1 to %d bytes", MaxCertificateName)
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c <= ' ' || c > '~' || c == '*' {
			return errors.New("a certificate's name is printable ASCII without a space or *")
		}
	}
	return nil
}

// CheckSPIFFEID returns nil when id is a SPIFFE ID with a path, spiffe://<trust domain>/<path>
// (SPIFFE ID §2): a trust domain of lowercase letters, digits, ".", "-" and "_", and one or more
// path segments of letters, digits, ".", "-" and "_", none "." or "..".
func CheckSPIFFEID(id string) error {
	return checkSPIFFE(id, false)
}

// checkSPIFFE is CheckSPIFFEID, and with pattern a last segment "*" too.
func checkSPIFFE(id string, pattern bool) error {
	rest, ok := strings.CutPrefix(id, SPIFFEScheme)
	if !ok || len(id) > maxSPIFFEID {
		return fmt.Errorf("a SPIFFE ID is %s<trust domain>/<path>, at most %d bytes", SPIFFEScheme, maxSPIFFEID)
	}
	td, path, ok := strings.Cut(rest, "/")
	if !trustDomainPattern.MatchString(td) {
		return errors.New("a SPIFFE trust domain is lowercase letters, digits, ., - and _")
	}
	if !ok || path == "" {
		return errors.New("a SPIFFE ID has a path")
	}
	segments := strings.Split(path, "/")
	for i, s := range segments {
		if pattern && s == "*" && i == len(segments)-1 {
			continue
		}
		if s == "." || s == ".." || !spiffeSegmentPattern.MatchString(s) {
			return errors.New("a segment of a SPIFFE ID's path is letters, digits, ., - and _, and is not . or .. alone")
		}
	}
	return nil
}

// CheckKubernetesNames returns nil when namespace is a DNS label of at most 63 bytes and
// serviceAccount a DNS subdomain of at most 253 (RFC 1123), the names Kubernetes gives them.
func CheckKubernetesNames(namespace, serviceAccount string) error {
	if len(namespace) > maxNamespace || !dnsLabelPattern.MatchString(namespace) {
		return fmt.Errorf("a namespace is a DNS label of at most %d bytes", maxNamespace)
	}
	if len(serviceAccount) > maxServiceAccount || !dnsSubdomainPattern.MatchString(serviceAccount) {
		return fmt.Errorf("a service account's name is a DNS subdomain of at most %d bytes", maxServiceAccount)
	}
	return nil
}

// checkWorkloadName returns nil when name is the name of a principal of the kind k, cert or k8s,
// or of a pattern of it, by docs/spec/rbac.md §9.
func checkWorkloadName(k Kind, name string) error {
	if k == KindKubernetes {
		return checkKubernetesName(name)
	}
	if strings.HasPrefix(name, SPIFFEScheme) {
		return checkSPIFFE(name, true)
	}
	bundle, value, ok := strings.Cut(name, "/")
	if !ok {
		return errors.New("want spiffe://<trust domain>/<path> or <bundle>/<name>")
	}
	if err := config.CheckIdentitySource(bundle); err != nil {
		return fmt.Errorf("the bundle: %w", err)
	}
	if value == "*" {
		return nil
	}
	return CheckCertificateName(strings.TrimSuffix(value, "/*"))
}

// checkKubernetesName checks <cluster>/<namespace>/<service account>, <cluster>/* and
// <cluster>/<namespace>/*.
func checkKubernetesName(name string) error {
	cluster, rest, ok := strings.Cut(name, "/")
	if !ok {
		return errors.New("want <cluster>/<namespace>/<service account>")
	}
	if err := config.CheckIdentitySource(cluster); err != nil {
		return fmt.Errorf("the cluster: %w", err)
	}
	if rest == "*" {
		return nil
	}
	namespace, sa, ok := strings.Cut(rest, "/")
	if !ok {
		return errors.New("want <cluster>/<namespace>/<service account>")
	}
	if sa == "*" {
		sa = "x" // a namespace's pattern: only the namespace is checked
	}
	return CheckKubernetesNames(namespace, sa)
}
