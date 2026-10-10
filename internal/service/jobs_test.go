package service_test

// Unit tests for the /jobs retrieval strategy using a fake store and a fake
// recommender. No database and no ML service needed.

import (
	"context"
	"errors"
	"testing"

	"github.com/hireonai/backend-hireon-go/internal/domain"
	"github.com/hireonai/backend-hireon-go/internal/repository/postgres"
	"github.com/hireonai/backend-hireon-go/internal/service"
)

type fakeStore struct {
	jobs       []domain.Job
	total      int
	profile    *domain.Profile
	profileErr error
	inOrder    []domain.Job
	filtered   map[string]bool
	lastFilter postgres.JobsFilter
}

func (f *fakeStore) ListJobs(_ context.Context, filter postgres.JobsFilter) ([]domain.Job, int, error) {
	f.lastFilter = filter
	return f.jobs, f.total, nil
}

func (f *fakeStore) ListJobsByPreference(_ context.Context, filter postgres.JobsFilter) ([]domain.Job, int, error) {
	f.lastFilter = filter
	return f.jobs, f.total, nil
}

func (f *fakeStore) ListJobsInOrder(context.Context, []string) ([]domain.Job, error) {
	return f.inOrder, nil
}

func (f *fakeStore) FilterJobIDs(_ context.Context, ids []string, _ postgres.JobsFilter) (map[string]bool, error) {
	if f.filtered != nil {
		return f.filtered, nil
	}
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

func (f *fakeStore) ProfileByUserID(context.Context, string) (*domain.Profile, error) {
	if f.profileErr != nil {
		return nil, f.profileErr
	}
	if f.profile == nil {
		return nil, domain.ErrNotFound
	}
	return f.profile, nil
}

func (f *fakeStore) CategoryIDsForNames(context.Context, []string) ([]string, error) {
	return []string{"cat-1"}, nil
}

func (f *fakeStore) AnalysisResultFor(context.Context, string, string) (*domain.AnalysisResult, error) {
	return nil, nil
}

func (f *fakeStore) JobByID(context.Context, string) (*domain.Job, error) {
	if len(f.jobs) == 0 {
		return nil, domain.ErrNotFound
	}
	return &f.jobs[0], nil
}

type fakeRecommender struct {
	recs []service.Recommendation
	err  error
}

func (f *fakeRecommender) Recommend(context.Context, string) ([]service.Recommendation, error) {
	return f.recs, f.err
}

func strptr(s string) *string { return &s }

// Anonymous callers get the plain listing.
func TestListJobsGuest(t *testing.T) {
	store := &fakeStore{jobs: []domain.Job{{ID: "j1"}}, total: 1}
	svc := service.NewJobsService(store, &fakeRecommender{})

	page, err := svc.ListJobs(context.Background(), nil, service.JobsQuery{Page: 1, Limit: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page.Jobs) != 1 || page.Pagination.TotalItems != 1 {
		t.Fatalf("unexpected page: %+v", page)
	}
	if page.Jobs[0].ScoreMatch != nil {
		t.Errorf("guest results must not carry a scoreMatch")
	}
}

// A user with no CV and no tag preferences falls back to the plain listing.
func TestListJobsWithoutCVOrTags(t *testing.T) {
	store := &fakeStore{
		jobs:    []domain.Job{{ID: "j1"}},
		total:   1,
		profile: &domain.Profile{ID: "p1", CVURL: nil, TagPreferences: nil},
	}
	svc := service.NewJobsService(store, &fakeRecommender{err: errors.New("must not be called")})

	if _, err := svc.ListJobs(context.Background(), &domain.User{ID: "u1"}, service.JobsQuery{Page: 1, Limit: 10}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A user with a CV is ranked by the recommendation engine.
func TestListJobsWithRecommendation(t *testing.T) {
	store := &fakeStore{
		profile: &domain.Profile{ID: "p1", CVURL: strptr("https://cdn/cv.pdf")},
		inOrder: []domain.Job{{ID: "j1"}, {ID: "j2"}},
	}
	rec := &fakeRecommender{recs: []service.Recommendation{
		{JobID: "j1", Score: 91.25},
		{JobID: "j2", Score: 42.5},
	}}
	svc := service.NewJobsService(store, rec)

	page, err := svc.ListJobs(context.Background(), &domain.User{ID: "u1"}, service.JobsQuery{Page: 1, Limit: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if page.Jobs[0].ScoreMatch == nil || *page.Jobs[0].ScoreMatch != "91.2%" {
		t.Errorf("score = %v, want 91.2%%", page.Jobs[0].ScoreMatch)
	}
}

// A failing ML service surfaces as an upstream error rather than a 200 with an
// empty list, which is what the Node implementation effectively produced.
func TestListJobsRecommendationFailure(t *testing.T) {
	store := &fakeStore{profile: &domain.Profile{ID: "p1", CVURL: strptr("https://cdn/cv.pdf")}}
	rec := &fakeRecommender{err: domain.ErrUpstreamSlow}
	svc := service.NewJobsService(store, rec)

	_, err := svc.ListJobs(context.Background(), &domain.User{ID: "u1"}, service.JobsQuery{Page: 1, Limit: 10})
	if !errors.Is(err, domain.ErrUpstreamSlow) {
		t.Fatalf("err = %v, want ErrUpstreamSlow", err)
	}
}

// Pagination slices the ranked list, not the raw corpus.
func TestRecommendationPagination(t *testing.T) {
	recs := make([]service.Recommendation, 25)
	jobs := make([]domain.Job, 25)
	for i := 0; i < 25; i++ {
		id := "job-" + string(rune('a'+i))
		recs[i] = service.Recommendation{JobID: id, Score: float64(100 - i)}
		jobs[i] = domain.Job{ID: id}
	}
	store := &fakeStore{
		profile: &domain.Profile{ID: "p1", CVURL: strptr("https://cdn/cv.pdf")},
		inOrder: jobs[10:20],
	}
	svc := service.NewJobsService(store, &fakeRecommender{recs: recs})

	page, err := svc.ListJobs(context.Background(), &domain.User{ID: "u1"}, service.JobsQuery{Page: 2, Limit: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if page.Pagination.TotalItems != 25 {
		t.Errorf("totalItems = %d, want 25", page.Pagination.TotalItems)
	}
	if page.Pagination.TotalPages != 3 || !page.Pagination.HasNextPage || !page.Pagination.HasPrevPage {
		t.Errorf("unexpected pagination: %+v", page.Pagination)
	}
}

// A malformed job id is rejected before any query runs.
func TestJobDetailRejectsMalformedID(t *testing.T) {
	svc := service.NewJobsService(&fakeStore{}, &fakeRecommender{})
	for _, id := range []string{"", "abc", "zzzzzzzzzzzzzzzzzzzzzzzz", "6ac3a7526853d5428be6acd2x"} {
		if _, err := svc.JobDetail(context.Background(), nil, id); err == nil {
			t.Errorf("id %q was accepted", id)
		}
	}
}

// A well-formed but absent id yields domain.ErrNotFound, which maps to 404. The
// Node backend returned 500 here.
func TestJobDetailNotFound(t *testing.T) {
	svc := service.NewJobsService(&fakeStore{}, &fakeRecommender{})
	_, err := svc.JobDetail(context.Background(), nil, "000000000000000000000000")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// An empty recommendation set yields an empty page, not an error.
func TestEmptyRecommendations(t *testing.T) {
	store := &fakeStore{profile: &domain.Profile{ID: "p1", CVURL: strptr("https://cdn/cv.pdf")}}
	svc := service.NewJobsService(store, &fakeRecommender{recs: nil})

	page, err := svc.ListJobs(context.Background(), &domain.User{ID: "u1"}, service.JobsQuery{Page: 1, Limit: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page.Jobs) != 0 || page.Pagination.TotalItems != 0 {
		t.Fatalf("unexpected page: %+v", page)
	}
}

// A page beyond the end returns an empty slice rather than panicking.
func TestPageBeyondEnd(t *testing.T) {
	store := &fakeStore{
		profile: &domain.Profile{ID: "p1", CVURL: strptr("https://cdn/cv.pdf")},
		inOrder: []domain.Job{},
	}
	svc := service.NewJobsService(store, &fakeRecommender{
		recs: []service.Recommendation{{JobID: "j1", Score: 1}},
	})
	page, err := svc.ListJobs(context.Background(), &domain.User{ID: "u1"}, service.JobsQuery{Page: 99, Limit: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page.Jobs) != 0 {
		t.Fatalf("expected no rows past the end, got %d", len(page.Jobs))
	}
}

func TestPaginationMath(t *testing.T) {
	cases := []struct {
		page, limit, total int
		totalPages         int
		hasNext, hasPrev   bool
	}{
		{1, 10, 0, 0, false, false},
		{1, 10, 1, 1, false, false},
		{1, 10, 10, 1, false, false},
		{1, 10, 11, 2, true, false},
		{2, 10, 11, 2, false, true},
		{1, 3, 2697, 899, true, false},
	}
	for _, tc := range cases {
		got := domain.NewPagination(tc.page, tc.limit, tc.total)
		if got.TotalPages != tc.totalPages || got.HasNextPage != tc.hasNext || got.HasPrevPage != tc.hasPrev {
			t.Errorf("page=%d limit=%d total=%d -> %+v", tc.page, tc.limit, tc.total, got)
		}
	}
}
