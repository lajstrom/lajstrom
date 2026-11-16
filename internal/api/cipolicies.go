package api

import (
	"net/http"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/ci"
	"github.com/lajstrom/lajstrom/internal/format"
)

// CIPolicyList is the answer of GET /api/v1/ci/policies: the CI trust policies of the
// configuration, in its order, then those of the API, by id (docs/spec/rbac.md §11).
type CIPolicyList struct {
	Policies []ci.Policy `json:"policies"`
}

// ciPolicies returns the service of the CI trust policies after the admin role: the role first,
// so that a principal without it learns nothing; 501 without the service.
func (h *handler) ciPolicies(r *http.Request) (*ci.Service, error) {
	if err := h.allowAdmin(r); err != nil {
		return nil, err
	}
	if h.ci == nil {
		return nil, &clientError{msg: "CI trust policies are not enabled in this server", err: format.ErrUnsupported}
	}
	return h.ci, nil
}

// listCIPolicies answers every CI trust policy, of the configuration and of the API.
func (h *handler) listCIPolicies(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	s, err := h.ciPolicies(r)
	if err != nil {
		return 0, nil, err
	}
	list, err := s.List(r.Context())
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, CIPolicyList{Policies: list}, nil
}

// getCIPolicy answers the CI trust policy of the path, one of the API with its ETag.
func (h *handler) getCIPolicy(w http.ResponseWriter, r *http.Request) (int, any, error) {
	s, err := h.ciPolicies(r)
	if err != nil {
		return 0, nil, err
	}
	p, err := s.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		return 0, nil, serviceError(err)
	}
	if p.Source == ci.SourceConfig {
		return http.StatusOK, p, nil
	}
	return tagged(w, r, ciPolicyETag(p), p)
}

// createCIPolicy makes a CI trust policy of the API from the body: 201 with it, its Location and
// its ETag.
func (h *handler) createCIPolicy(w http.ResponseWriter, r *http.Request) (int, any, error) {
	s, err := h.ciPolicies(r)
	if err != nil {
		return 0, nil, err
	}
	var req ci.Policy
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	p, err := s.Create(r.Context(), req, auth.Origin(origin(r)))
	if err != nil {
		return 0, nil, serviceError(err)
	}
	w.Header().Set("Location", "/api/v1/ci/policies/"+p.Name)
	w.Header().Set("ETag", ciPolicyETag(p))
	return http.StatusCreated, p, nil
}

// replaceCIPolicy replaces the issuer, the claims, the grants and the expiry of the CI trust policy
// of the path by the body's: 200 with it and its ETag; 409 for a policy of the configuration.
// With If-Match, the change is made only while the policy has a revision it names.
func (h *handler) replaceCIPolicy(w http.ResponseWriter, r *http.Request) (int, any, error) {
	s, r, err := h.conditionalCIPolicy(r)
	if err != nil {
		return 0, nil, err
	}
	var req ci.Policy
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	p, err := s.Update(r.Context(), r.PathValue("name"), req, auth.Origin(origin(r)))
	return answerCIPolicy(w, p, err)
}

// CIPolicyRenewal is the body of POST /api/v1/ci/policies/{name}/renew, which may be left out:
// the expiry asked, 180 days ahead when it is absent.
type CIPolicyRenewal struct {
	ExpiresAt *time.Time `json:"expiresAt"`
}

// renewCIPolicy renews the CI trust policy of the path, one of the API that grants write: 200
// with it and its ETag. With If-Match, only while the policy has a revision it names.
func (h *handler) renewCIPolicy(w http.ResponseWriter, r *http.Request) (int, any, error) {
	s, r, err := h.conditionalCIPolicy(r)
	if err != nil {
		return 0, nil, err
	}
	var req CIPolicyRenewal
	if err := decodeOptionalBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	p, err := s.Renew(r.Context(), r.PathValue("name"), req.ExpiresAt, auth.Origin(origin(r)))
	return answerCIPolicy(w, p, err)
}

// approveCIPolicy approves what waits on the CI trust policy of the path: 200 with it and its
// ETag. It takes no body, and If-Match with the ETag of the revision approved: without one, or
// with *, it is 400.
func (h *handler) approveCIPolicy(w http.ResponseWriter, r *http.Request) (int, any, error) {
	s, r, err := h.conditionalCIPolicy(r)
	if err != nil {
		return 0, nil, err
	}
	p, err := s.Approve(r.Context(), r.PathValue("name"), auth.Origin(origin(r)))
	return answerCIPolicy(w, p, err)
}

// deleteCIPolicy deletes the CI trust policy of the path: 204; 409 for a policy of the
// configuration. With If-Match, only while the policy has a revision it names.
func (h *handler) deleteCIPolicy(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	s, r, err := h.conditionalCIPolicy(r)
	if err != nil {
		return 0, nil, err
	}
	if err := s.Delete(r.Context(), r.PathValue("name"), auth.Origin(origin(r))); err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusNoContent, nil, nil
}

// conditionalCIPolicy returns the service of the CI trust policies after the admin role, and r
// with the condition of its If-Match on the policy of the path (conditional).
func (h *handler) conditionalCIPolicy(r *http.Request) (*ci.Service, *http.Request, error) {
	s, err := h.ciPolicies(r)
	if err != nil {
		return nil, r, err
	}
	r, err = conditional(r, TableCIPolicies, "CI trust policy "+r.PathValue("name"))
	return s, r, err
}

// answerCIPolicy answers the policy p of a change, 200 with its ETag, or the error of the change.
func answerCIPolicy(w http.ResponseWriter, p *ci.Policy, err error) (int, any, error) {
	if err != nil {
		return 0, nil, serviceError(err)
	}
	w.Header().Set("ETag", ciPolicyETag(p))
	return http.StatusOK, p, nil
}

// ciPolicyETag is the ETag of the policy of the API p.
func ciPolicyETag(p *ci.Policy) string { return ETag(TableCIPolicies, p.ID, p.Revision) }
