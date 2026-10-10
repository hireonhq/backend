// Package service holds business logic. Nothing here imports net/http: functions
// take a context and return values or sentinel errors, so the HTTP layer owns all
// transport concerns. In the Node backend a service function referenced the Hapi
// response toolkit `h` it never received, which is what that separation prevents.
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/hireonai/backend-hireon-go/internal/domain"
	"github.com/hireonai/backend-hireon-go/internal/repository/postgres"
)

// ReferenceStore is the persistence surface needed by the reference endpoints.
type ReferenceStore interface {
	ListJobCategories(ctx context.Context) ([]domain.JobCategory, error)
	ListJobMinExperiences(ctx context.Context) ([]domain.JobMinExperience, error)
	ListCompanyIndustries(ctx context.Context) ([]domain.CompanyIndustry, error)
}

type ReferenceService struct {
	store ReferenceStore
}

func NewReferenceService(store ReferenceStore) *ReferenceService {
	return &ReferenceService{store: store}
}

// Empty reference collections return an empty list, not an error. The Node
// handlers threw on an empty result, which the catch turned into a 500.
func (s *ReferenceService) JobCategories(ctx context.Context) ([]domain.JobCategory, error) {
	return s.store.ListJobCategories(ctx)
}

func (s *ReferenceService) JobMinExperiences(ctx context.Context) ([]domain.JobMinExperience, error) {
	return s.store.ListJobMinExperiences(ctx)
}

func (s *ReferenceService) CompanyIndustries(ctx context.Context) ([]domain.CompanyIndustry, error) {
	return s.store.ListCompanyIndustries(ctx)
}

// JobsService serves GET /jobs and GET /jobs/{id}.
// JobStore is the persistence surface the service depends on. Depending on an
// interface rather than the concrete store keeps business logic testable without
// a database -- the Node services imported models directly, so any test needed a
// live Mongo.
type JobStore interface {
	ListJobs(ctx context.Context, f postgres.JobsFilter) ([]domain.Job, int, error)
	ListJobsByPreference(ctx context.Context, f postgres.JobsFilter) ([]domain.Job, int, error)
	ListJobsInOrder(ctx context.Context, ids []string) ([]domain.Job, error)
	FilterJobIDs(ctx context.Context, ids []string, f postgres.JobsFilter) (map[string]bool, error)
	ProfileByUserID(ctx context.Context, userID string) (*domain.Profile, error)
	CategoryIDsForNames(ctx context.Context, patterns []string) ([]string, error)
	JobByID(ctx context.Context, id string) (*domain.Job, error)
	AnalysisResultFor(ctx context.Context, userID, jobID string) (*domain.AnalysisResult, error)
}

type JobsService struct {
	store JobStore
	ml    Recommender
}

// Recommender abstracts the recommendation engine so the service can be tested
// without a live ML service.
type Recommender interface {
	Recommend(ctx context.Context, userID string) ([]Recommendation, error)
}

type Recommendation struct {
	JobID string
	Score float64
}

func NewJobsService(store JobStore, ml Recommender) *JobsService {
	return &JobsService{store: store, ml: ml}
}

// JobsQuery is the validated query for GET /jobs.
type JobsQuery struct {
	Keyword    string
	MinSalary  *int64
	MaxSalary  *int64
	Experience string
	Categories []string
	Industry   string
	Page       int
	Limit      int
}

type JobsPage struct {
	Jobs       []domain.Job
	Pagination domain.Pagination
}

// ListJobs picks the retrieval strategy. Three modes existed in one Node function
// spread over 594 lines; each mode now names itself.
func (s *JobsService) ListJobs(ctx context.Context, user *domain.User, q JobsQuery) (*JobsPage, error) {
	filter := postgres.JobsFilter{
		Keyword:      q.Keyword,
		MinSalary:    q.MinSalary,
		MaxSalary:    q.MaxSalary,
		ExperienceID: q.Experience,
		CategoryIDs:  q.Categories,
		IndustryID:   q.Industry,
		Page:         q.Page,
		Limit:        q.Limit,
	}

	// Anonymous visitors and users without preferences get a plain listing.
	if user == nil {
		jobs, total, err := s.store.ListJobs(ctx, filter)
		if err != nil {
			return nil, err
		}
		return &JobsPage{Jobs: withNullScore(jobs), Pagination: domain.NewPagination(q.Page, q.Limit, total)}, nil
	}

	profile, err := s.store.ProfileByUserID(ctx, user.ID)
	if err != nil && err != domain.ErrNotFound {
		return nil, err
	}

	var tags []string
	if profile != nil {
		tags = profile.TagPreferences
	}

	// Users without a CV fall back to tag-preference matching, which is plain
	// SQL; only users with a CV trigger the recommendation engine.
	if profile == nil || profile.CVURL == nil || *profile.CVURL == "" {
		if len(tags) == 0 {
			jobs, total, err := s.store.ListJobs(ctx, filter)
			if err != nil {
				return nil, err
			}
			return &JobsPage{Jobs: withNullScore(jobs), Pagination: domain.NewPagination(q.Page, q.Limit, total)}, nil
		}
		return s.listByPreferences(ctx, tags, filter, q)
	}

	return s.listByRecommendation(ctx, user.ID, filter, q)
}

// listByPreferences ranks nothing; it just narrows by tag match. The tags are
// matched against category names and text columns, the same fields the Node
// implementation used.
func (s *JobsService) listByPreferences(ctx context.Context, tags []string, filter postgres.JobsFilter, q JobsQuery) (*JobsPage, error) {
	patterns := make([]string, 0, len(tags))
	for _, t := range tags {
		patterns = append(patterns, "%"+t+"%")
	}
	catIDs, err := s.store.CategoryIDsForNames(ctx, patterns)
	if err != nil {
		return nil, err
	}
	filter.PreferenceTags = tags
	filter.PreferenceCategoryIDs = catIDs

	jobs, total, err := s.store.ListJobsByPreference(ctx, filter)
	if err != nil {
		return nil, err
	}
	return &JobsPage{Jobs: withNullScore(jobs), Pagination: domain.NewPagination(q.Page, q.Limit, total)}, nil
}

// listByRecommendation uses the ML ranking, then narrows to jobs that still match
// the user's filters.
func (s *JobsService) listByRecommendation(ctx context.Context, userID string, filter postgres.JobsFilter, q JobsQuery) (*JobsPage, error) {
	recs, err := s.ml.Recommend(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return &JobsPage{
			Jobs:       []domain.Job{},
			Pagination: domain.NewPagination(q.Page, q.Limit, 0),
		}, nil
	}

	ids := make([]string, 0, len(recs))
	scoreByID := make(map[string]float64, len(recs))
	for _, r := range recs {
		ids = append(ids, r.JobID)
		scoreByID[r.JobID] = r.Score
	}

	// One query narrows the ranked ids by the active filters. The Node version
	// fetched every recommended job and intersected in application code.
	if q.hasFilters() {
		allowed, err := s.store.FilterJobIDs(ctx, ids, filter)
		if err != nil {
			return nil, err
		}
		kept := ids[:0:0]
		for _, id := range ids {
			if allowed[id] {
				kept = append(kept, id)
			}
		}
		ids = kept
	}

	total := len(ids)
	start := (q.Page - 1) * q.Limit
	if start > total {
		start = total
	}
	end := start + q.Limit
	if end > total {
		end = total
	}
	pageIDs := ids[start:end]

	jobs, err := s.store.ListJobsInOrder(ctx, pageIDs)
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		if score, ok := scoreByID[jobs[i].ID]; ok {
			label := formatScore(score)
			jobs[i].ScoreMatch = &label
		}
	}
	return &JobsPage{Jobs: jobs, Pagination: domain.NewPagination(q.Page, q.Limit, total)}, nil
}

func (q JobsQuery) hasFilters() bool {
	return q.Keyword != "" || q.MinSalary != nil || q.MaxSalary != nil ||
		q.Experience != "" || len(q.Categories) > 0 || q.Industry != ""
}

// formatScore renders a similarity as a percentage with one decimal, matching the
// Node output (e.g. "85.0%").
func formatScore(score float64) string {
	return fmt.Sprintf("%.1f%%", score)
}

func withNullScore(jobs []domain.Job) []domain.Job {
	for i := range jobs {
		jobs[i].ScoreMatch = nil
	}
	return jobs
}

// JobDetail returns one job plus the caller's stored analysis, if any.
func (s *JobsService) JobDetail(ctx context.Context, user *domain.User, jobID string) (*domain.JobDetail, error) {
	if !looksLikeID(jobID) {
		return nil, domain.NewAPIError(400, "Invalid job id.", nil)
	}
	job, err := s.store.JobByID(ctx, jobID)
	if err != nil {
		return nil, err
	}
	detail := &domain.JobDetail{Job: *job, AnalysisResult: nil}
	if user != nil {
		res, err := s.store.AnalysisResultFor(ctx, user.ID, jobID)
		if err != nil {
			return nil, err
		}
		detail.AnalysisResult = res
	}
	return detail, nil
}

// looksLikeID rejects ids that cannot match the 24-hex-character id format, so an
// obviously invalid id is a 400 rather than a database round trip. The Node
// version dereferenced a nil job before checking it, returning 500 for a valid
// but absent id; this returns domain.ErrNotFound, mapped to 404.
func looksLikeID(id string) bool {
	if len(id) != 24 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

var _ = strings.TrimSpace
