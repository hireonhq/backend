package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hireonai/backend-hireon-go/internal/httpx"
	"github.com/hireonai/backend-hireon-go/internal/service"
)

type Jobs struct {
	svc *service.JobsService
}

func NewJobs(svc *service.JobsService) *Jobs { return &Jobs{svc: svc} }

// List handles GET /jobs.
//
// Query parsing reproduces the Joi rules from the Node backend, including their
// rejection behaviour: page must be >= 1, limit between 1 and 100, and min/max
// salary must be numeric. Invalid input yields 400 with the same message text.
func (h *Jobs) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	page, ok := intParam(q.Get("page"), 1)
	if !ok || page < 1 {
		httpx.BadRequest(w, "Invalid request query input")
		return
	}
	limit, ok := intParam(q.Get("limit"), 10)
	if !ok || limit < 1 || limit > 100 {
		httpx.BadRequest(w, "Invalid request query input")
		return
	}

	query := service.JobsQuery{
		Keyword:    strings.TrimSpace(q.Get("keyword")),
		Experience: strings.TrimSpace(q.Get("experience")),
		Industry:   strings.TrimSpace(q.Get("industry")),
		Page:       page,
		Limit:      limit,
	}

	if v := strings.TrimSpace(q.Get("minSalary")); v != "" {
		n, ok := int64Param(v)
		if !ok {
			httpx.BadRequest(w, "Invalid request query input")
			return
		}
		query.MinSalary = &n
	}
	if v := strings.TrimSpace(q.Get("maxSalary")); v != "" {
		n, ok := int64Param(v)
		if !ok {
			httpx.BadRequest(w, "Invalid request query input")
			return
		}
		query.MaxSalary = &n
	}
	if v := strings.TrimSpace(q.Get("category")); v != "" {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				query.Categories = append(query.Categories, part)
			}
		}
	}

	page_, err := h.svc.ListJobs(r.Context(), httpx.UserFrom(r.Context()), query)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, map[string]any{
		"jobs":       page_.Jobs,
		"pagination": page_.Pagination,
	}, "Jobs successfully retrieved.")
}

// Detail handles GET /jobs/{jobId}.
func (h *Jobs) Detail(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")
	detail, err := h.svc.JobDetail(r.Context(), httpx.UserFrom(r.Context()), jobID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, detail, "Job details successfully retrieved.")
}

func intParam(raw string, fallback int) (int, bool) {
	if strings.TrimSpace(raw) == "" {
		return fallback, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return n, true
}

func int64Param(raw string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
