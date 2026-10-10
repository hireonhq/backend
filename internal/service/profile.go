package service

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/hireonai/backend-hireon-go/internal/domain"
	"github.com/hireonai/backend-hireon-go/internal/storage"
)

// Upload limits, matching the Joi rules and the service-level checks in the Node
// backend (which duplicated the same 5 MB constant in three places).
const (
	MaxPhotoBytes = 5 * 1024 * 1024
	MaxCVBytes    = 5 * 1024 * 1024
)

var (
	allowedPhotoTypes = map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
		"image/gif":  ".gif",
	}
	allowedCVTypes = map[string]string{
		"application/pdf": ".pdf",
	}
)

// ProfileStore is the persistence surface needed by the profile endpoints.
type ProfileStore interface {
	ProfileByUserID(ctx context.Context, userID string) (*domain.Profile, error)
	UpdateProfileFields(ctx context.Context, userID string, fields map[string]any) error
	UpdateTagPreferences(ctx context.Context, userID string, tags []string) error
	AddBookmark(ctx context.Context, userID, jobID string) error
	RemoveBookmark(ctx context.Context, userID, jobID string) error
	BookmarkedJobs(ctx context.Context, userID string) ([]domain.Job, error)
	SetPhoto(ctx context.Context, userID string, url string) error
	SetCV(ctx context.Context, userID string, cvURL *string, status string) error
	ProfileCV(ctx context.Context, userID string) (string, string, error)
}

type ProfileService struct {
	store   ProfileStore
	objects storage.Store
	queue   Enqueuer
}

// Enqueuer records background work durably. The first version of this service
// spawned a goroutine instead, so a deploy or crash discarded the work.
type Enqueuer interface {
	Enqueue(ctx context.Context, userID, kind string) (string, error)
}

// CVEmbedder builds and stores a CV embedding. Implemented by the ML client and
// used by the worker, not by the request path.
type CVEmbedder interface {
	EmbedCV(ctx context.Context, userID, cvURL string) error
}

func NewProfileService(store ProfileStore, objects storage.Store, queue Enqueuer) *ProfileService {
	return &ProfileService{store: store, objects: objects, queue: queue}
}

// EnqueueCVEmbedding records the embedding work durably. The response returns as
// soon as the row is written; a worker picks it up, retries it on failure, and
// survives a restart.
func (s *ProfileService) EnqueueCVEmbedding(ctx context.Context, userID string) (string, error) {
	if s.queue == nil {
		return "", nil
	}
	return s.queue.Enqueue(ctx, userID, "cv_embedding")
}

func (s *ProfileService) Get(ctx context.Context, userID string) (*domain.Profile, error) {
	return s.store.ProfileByUserID(ctx, userID)
}

type ProfileUpdate struct {
	Fullname      *string
	Phone         *string
	Domicile      *string
	LastEducation *string
	PortfolioURL  *string
}

func (s *ProfileService) Update(ctx context.Context, userID string, in ProfileUpdate) (*domain.Profile, error) {
	if in.Fullname != nil && strings.TrimSpace(*in.Fullname) == "" {
		return nil, domain.NewAPIError(400, "Fullname must not be empty.", nil)
	}
	fields := map[string]any{}
	if in.Fullname != nil {
		fields["fullname"] = *in.Fullname
	}
	if in.Phone != nil {
		fields["phone"] = *in.Phone
	}
	if in.Domicile != nil {
		fields["domicile"] = *in.Domicile
	}
	if in.LastEducation != nil {
		fields["last_education"] = *in.LastEducation
	}
	if in.PortfolioURL != nil {
		fields["portfolio_url"] = *in.PortfolioURL
	}
	if err := s.store.UpdateProfileFields(ctx, userID, fields); err != nil {
		return nil, err
	}
	return s.store.ProfileByUserID(ctx, userID)
}

func (s *ProfileService) UpdateTagPreferences(ctx context.Context, userID string, tags []string) ([]string, error) {
	if len(tags) == 0 {
		return nil, domain.NewAPIError(400, "Tag preferences is required.", nil)
	}
	if err := s.store.UpdateTagPreferences(ctx, userID, tags); err != nil {
		return nil, err
	}
	return tags, nil
}

func (s *ProfileService) AddBookmark(ctx context.Context, userID, jobID string) ([]domain.Job, error) {
	if !looksLikeID(jobID) {
		return nil, domain.NewAPIError(400, "Invalid job id.", nil)
	}
	if err := s.store.AddBookmark(ctx, userID, jobID); err != nil {
		return nil, err
	}
	return s.store.BookmarkedJobs(ctx, userID)
}

func (s *ProfileService) RemoveBookmark(ctx context.Context, userID, jobID string) ([]domain.Job, error) {
	if !looksLikeID(jobID) {
		return nil, domain.NewAPIError(400, "Invalid job id.", nil)
	}
	if err := s.store.RemoveBookmark(ctx, userID, jobID); err != nil {
		return nil, err
	}
	return s.store.BookmarkedJobs(ctx, userID)
}

// UploadPhoto stores a new photo and then removes the previous one.
//
// Ordering matters: the Node backend deleted the old object *before* uploading
// the replacement, so a failed upload destroyed the user's existing photo while
// the database still pointed at it.
func (s *ProfileService) UploadPhoto(ctx context.Context, userID string, r io.Reader, filename, contentType string) (string, error) {
	ext, ok := allowedPhotoTypes[contentType]
	if !ok {
		return "", domain.NewAPIError(400, "Invalid photo type. Allowed types are JPEG, PNG, GIF.", nil)
	}
	body, err := readLimited(r, MaxPhotoBytes)
	if err != nil {
		return "", err
	}

	profile, err := s.store.ProfileByUserID(ctx, userID)
	if err != nil {
		return "", err
	}
	previous := profile.PhotoURL

	key := fmt.Sprintf("photos/%s/%s%s", userID, sanitizeName(filename), ext)
	url, err := s.objects.Put(ctx, key, strings.NewReader(string(body)), contentType)
	if err != nil {
		return "", domain.ErrUpstream
	}
	if err := s.store.SetPhoto(ctx, userID, url); err != nil {
		return "", err
	}
	if previous != nil && *previous != "" {
		// Best effort: the new photo is already live, so a cleanup failure must
		// not fail the request.
		_ = s.objects.Delete(ctx, *previous)
	}
	return url, nil
}

// UploadCV stores the PDF and records that it is awaiting embedding.
//
// The Node backend uploaded, then synchronously called the ML service to build an
// embedding and only afterwards saved cvUrl -- with no timeout, so a slow ML
// service left the request hanging. Here the upload completes and the embedding
// becomes queued work, so the response does not depend on the ML service.
func (s *ProfileService) UploadCV(
	ctx context.Context,
	userID string,
	r io.Reader,
	filename, contentType string,
	enqueue func(ctx context.Context, userID string) (string, error),
) (cvURL string, jobID string, err error) {
	ext, ok := allowedCVTypes[contentType]
	if !ok {
		return "", "", domain.NewAPIError(400, "Invalid CV type. Allowed types are PDF.", nil)
	}
	body, err := readLimited(r, MaxCVBytes)
	if err != nil {
		return "", "", err
	}

	profile, err := s.store.ProfileByUserID(ctx, userID)
	if err != nil {
		return "", "", err
	}
	previous := profile.CVURL

	key := fmt.Sprintf("user_cv/%s/%s%s", userID, sanitizeName(filename), ext)
	url, err := s.objects.Put(ctx, key, strings.NewReader(string(body)), contentType)
	if err != nil {
		return "", "", domain.ErrUpstream
	}
	if err := s.store.SetCV(ctx, userID, &url, "pending"); err != nil {
		return "", "", err
	}
	if previous != nil && *previous != "" {
		_ = s.objects.Delete(ctx, *previous)
	}

	jobID, err = enqueue(ctx, userID)
	if err != nil {
		// The CV is stored and usable; only the embedding is pending. Report the
		// upload as successful rather than losing it.
		return url, "", nil
	}
	return url, jobID, nil
}

// DeleteCV clears the stored CV and removes the object.
func (s *ProfileService) DeleteCV(ctx context.Context, userID string) error {
	cvURL, _, err := s.store.ProfileCV(ctx, userID)
	if err != nil {
		return err
	}
	if cvURL == "" {
		return domain.NewAPIError(404, "CV not found.", nil)
	}
	if err := s.store.SetCV(ctx, userID, nil, "none"); err != nil {
		return err
	}
	_ = s.objects.Delete(ctx, cvURL)
	return nil
}

// CVAnalyzer forwards a general CV analysis to the ML service.
type CVAnalyzer interface {
	AnalyzeGeneralCV(ctx context.Context, data []byte, filename, contentType string) (map[string]any, error)
}

// AnalyzeCV validates the upload before forwarding it.
func (s *ProfileService) AnalyzeCV(ctx context.Context, analyzer CVAnalyzer, r io.Reader, filename, contentType string) (map[string]any, error) {
	if _, ok := allowedCVTypes[contentType]; !ok {
		return nil, domain.NewAPIError(400, "Invalid CV type. Allowed types are PDF.", nil)
	}
	body, err := readLimited(r, MaxCVBytes)
	if err != nil {
		return nil, err
	}
	return analyzer.AnalyzeGeneralCV(ctx, body, filename, contentType)
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, domain.NewAPIError(400, "Could not read the uploaded file.", err)
	}
	if int64(len(body)) > max {
		return nil, domain.NewAPIError(400, fmt.Sprintf("File size must not be larger than %dMB.", max/(1024*1024)), nil)
	}
	if len(body) == 0 {
		return nil, domain.NewAPIError(400, "Uploaded file is empty.", nil)
	}
	return body, nil
}

// sanitizeName strips directory components and whitespace so a hostile filename
// cannot escape the intended prefix. filepath.Base also defeats "..".
func sanitizeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, " ", "-")
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}
