package api

import (
	"net/http"

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

// getCIPolicy answers the CI trust policy of the path.
func (h *handler) getCIPolicy(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	s, err := h.ciPolicies(r)
	if err != nil {
		return 0, nil, err
	}
	p, err := s.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusOK, p, nil
}

// createCIPolicy makes a CI trust policy of the API from the body: 201 with it and its Location.
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
	return http.StatusCreated, p, nil
}

// replaceCIPolicy replaces the issuer, the claims and the grants of the CI trust policy of the
// path by the body's; 409 for a policy of the configuration.
func (h *handler) replaceCIPolicy(w http.ResponseWriter, r *http.Request) (int, any, error) {
	s, err := h.ciPolicies(r)
	if err != nil {
		return 0, nil, err
	}
	var req ci.Policy
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	p, err := s.Update(r.Context(), r.PathValue("name"), req, auth.Origin(origin(r)))
	if err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusOK, p, nil
}

// deleteCIPolicy deletes the CI trust policy of the path: 204; 409 for a policy of the
// configuration.
func (h *handler) deleteCIPolicy(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	s, err := h.ciPolicies(r)
	if err != nil {
		return 0, nil, err
	}
	if err := s.Delete(r.Context(), r.PathValue("name"), auth.Origin(origin(r))); err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusNoContent, nil, nil
}
