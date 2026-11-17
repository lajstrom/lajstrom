package auth

import "slices"

// The access report and its review (docs/spec/rbac.md §14, #1279): who has what at one moment,
// and the record that a person reviewed it.

// The scopes of the access report and of its review (docs/spec/rbac.md §14): AccessAll, every
// section; AccessPeople, the users of kind local and oidc and the groups, for PCI DSS 7.2.4;
// AccessSystems, the service accounts, the workloads, the deploy tokens and the CI trust
// policies, for 7.2.5.1. The anonymous reads are in every scope.
const (
	AccessAll     = "all"
	AccessPeople  = "people"
	AccessSystems = "systems"
)

// AccessScopes are the scopes, in the order the messages name them.
var AccessScopes = []string{AccessAll, AccessPeople, AccessSystems}

// CheckAccessScope returns an error wrapping ErrInvalid, naming the field scope, unless scope is
// one of AccessScopes.
func CheckAccessScope(scope string) error {
	if !slices.Contains(AccessScopes, scope) {
		return invalidf("scope: %q is not all, people or systems", scope)
	}
	return nil
}
