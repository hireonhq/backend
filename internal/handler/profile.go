package handler

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hireonai/backend-hireon-go/internal/domain"
	"github.com/hireonai/backend-hireon-go/internal/httpx"
	"github.com/hireonai/backend-hireon-go/internal/service"
)

// JobStatusReader reports background job state.
type JobStatusReader interface {
	QueueStatus(ctx context.Context, userID, jobID string) (*domain.QueuedJobStatus, error)
}

type Profile struct {
	svc    *service.ProfileService
	status JobStatusReader
}

func NewProfile(svc *service.ProfileService, status JobStatusReader) *Profile {
	return &Profile{svc: svc, status: status}
}

// requireUser is the single place handlers read the authenticated user. Every
// route behind RequireAuth has one, so a nil user here is a wiring bug and the
// middleware is the guarantee.
func requireUser(w http.ResponseWriter, r *http.Request) (*domain.User, bool) {
	user := httpx.UserFrom(r.Context())
	if user == nil {
		httpx.Error(w, http.StatusUnauthorized, "Invalid or expired token.")
		return nil, false
	}
	return user, true
}

func (h *Profile) Get(w http.ResponseWriter, r *http.Request) {
	user, ok := requireUser(w, r)
	if !ok {
		return
	}
	profile, err := h.svc.Get(r.Context(), user.ID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, profile, "Profile successfully retrieved.")
}

type updateProfileBody struct {
	Fullname      *string `json:"fullname"`
	Phone         *string `json:"phone"`
	Domicile      *string `json:"domicile"`
	LastEducation *string `json:"lastEducation"`
	PortfolioURL  *string `json:"portfolioUrl"`
}

func (h *Profile) Update(w http.ResponseWriter, r *http.Request) {
	user, ok := requireUser(w, r)
	if !ok {
		return
	}
	var body updateProfileBody
	if !decodeJSON(w, r, &body, 1<<20) {
		return
	}
	profile, err := h.svc.Update(r.Context(), user.ID, service.ProfileUpdate{
		Fullname:      body.Fullname,
		Phone:         body.Phone,
		Domicile:      body.Domicile,
		LastEducation: body.LastEducation,
		PortfolioURL:  body.PortfolioURL,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, profile, "Profile successfully updated")
}

func (h *Profile) UpdateTagPreferences(w http.ResponseWriter, r *http.Request) {
	user, ok := requireUser(w, r)
	if !ok {
		return
	}
	var body struct {
		TagPreferences []string `json:"tagPreferences"`
	}
	if !decodeJSON(w, r, &body, 1<<20) {
		return
	}
	tags, err := h.svc.UpdateTagPreferences(r.Context(), user.ID, body.TagPreferences)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, map[string]any{"updatedTagPreferences": tags},
		"Tag preferences successfully updated.")
}

func (h *Profile) AddBookmark(w http.ResponseWriter, r *http.Request) {
	user, ok := requireUser(w, r)
	if !ok {
		return
	}
	var body struct {
		JobID string `json:"jobId"`
	}
	if !decodeJSON(w, r, &body, 1<<20) {
		return
	}
	if body.JobID == "" {
		httpx.BadRequest(w, "Invalid request payload input")
		return
	}
	jobs, err := h.svc.AddBookmark(r.Context(), user.ID, body.JobID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, map[string]any{"updatedBookmarkJobs": jobs}, "Job successfully bookmarked")
}

func (h *Profile) DeleteBookmark(w http.ResponseWriter, r *http.Request) {
	user, ok := requireUser(w, r)
	if !ok {
		return
	}
	jobs, err := h.svc.RemoveBookmark(r.Context(), user.ID, chi.URLParam(r, "jobId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, map[string]any{"updatedBookmarkJobs": jobs}, "Job successfully removed from bookmark")
}

// UploadPhoto and UploadCV accept multipart/form-data with a single file field.
func (h *Profile) UploadPhoto(w http.ResponseWriter, r *http.Request) {
	user, ok := requireUser(w, r)
	if !ok {
		return
	}
	file, header, err := multipartFile(r, "photo")
	if err != nil {
		httpx.BadRequest(w, "Invalid request payload input")
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	url, err := h.svc.UploadPhoto(r.Context(), user.ID, file, header.Filename, contentType)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, map[string]any{"photoUrl": url}, "Profile photo uploaded successfully.")
}

func (h *Profile) UploadCV(w http.ResponseWriter, r *http.Request) {
	user, ok := requireUser(w, r)
	if !ok {
		return
	}
	file, header, err := multipartFile(r, "cv")
	if err != nil {
		httpx.BadRequest(w, "Invalid request payload input")
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	cvURL, jobID, err := h.svc.UploadCV(r.Context(), user.ID, file, header.Filename, contentType,
		h.svc.EnqueueCVEmbedding)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, map[string]any{"cvUrl": cvURL, "jobId": jobID},
		"CV uploaded successfully. Embedding is being processed.")
}

// JobStatus reports the state of a background job owned by the caller. The row is
// the source of truth, so the status is correct even if this process restarted
// while the job was in flight.
func (h *Profile) JobStatus(w http.ResponseWriter, r *http.Request) {
	user, ok := requireUser(w, r)
	if !ok {
		return
	}
	status, err := h.status.QueueStatus(r.Context(), user.ID, chi.URLParam(r, "jobId"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, status, "Job status successfully retrieved.")
}

func (h *Profile) DeleteCV(w http.ResponseWriter, r *http.Request) {
	user, ok := requireUser(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteCV(r.Context(), user.ID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Success(w, nil, "CV successfully deleted.")
}
