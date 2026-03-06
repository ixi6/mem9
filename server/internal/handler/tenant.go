package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/qiffang/mnemos/server/internal/service"
)

type registerTenantRequest struct {
	Name string `json:"name"`
}

type registerTenantResponse struct {
	OK       bool   `json:"ok"`
	TenantID string `json:"tenant_id"`
	Token    string `json:"token,omitempty"`
	ClaimURL string `json:"claim_url,omitempty"`
	Status   string `json:"status"`
}

func (s *Server) registerTenant(w http.ResponseWriter, r *http.Request) {
	var req registerTenantRequest
	if err := decode(r, &req); err != nil {
		s.handleError(w, err)
		return
	}

	result, err := s.tenant.Register(r.Context(), service.RegisterInput{
		Name: req.Name,
	})
	if err != nil {
		s.handleError(w, err)
		return
	}

	respond(w, http.StatusCreated, registerTenantResponse{
		OK:       true,
		TenantID: result.TenantID,
		Token:    result.Token,
		ClaimURL: result.ClaimURL,
		Status:   string(result.Status),
	})
}

type addTenantTokenResponse struct {
	OK       bool   `json:"ok"`
	APIToken string `json:"api_token"`
}

func (s *Server) addTenantToken(w http.ResponseWriter, r *http.Request) {
	auth := authInfo(r)
	tenantID := chi.URLParam(r, "tenantID")

	// Verify the caller belongs to this tenant.
	if auth.TenantID != tenantID {
		respondError(w, http.StatusForbidden, "cannot add token to a different tenant")
		return
	}

	token, err := s.tenant.AddToken(r.Context(), tenantID)
	if err != nil {
		s.handleError(w, err)
		return
	}

	respond(w, http.StatusCreated, addTenantTokenResponse{OK: true, APIToken: token})
}

func (s *Server) getTenantInfo(w http.ResponseWriter, r *http.Request) {
	auth := authInfo(r)
	tenantID := chi.URLParam(r, "tenantID")

	// Verify the caller belongs to this tenant.
	if auth.TenantID != tenantID {
		respondError(w, http.StatusForbidden, "cannot view a different tenant")
		return
	}

	info, err := s.tenant.GetInfo(r.Context(), tenantID)
	if err != nil {
		s.handleError(w, err)
		return
	}

	respond(w, http.StatusOK, info)
}
