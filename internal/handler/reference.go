package handler

import (
	"net/http"

	"github.com/hireonai/backend-hireon-go/internal/httpx"
	"github.com/hireonai/backend-hireon-go/internal/service"
)

type Reference struct {
	svc *service.ReferenceService
}

func NewReference(svc *service.ReferenceService) *Reference { return &Reference{svc: svc} }

func (h *Reference) JobCategories(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.JobCategories(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, items, "Job categories successfully retrieved.")
}

func (h *Reference) JobMinExperiences(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.JobMinExperiences(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, items, "Job minimum experiences successfully retrieved.")
}

func (h *Reference) CompanyIndustries(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.CompanyIndustries(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, items, "Company industries successfully retrieved.")
}
