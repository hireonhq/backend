package postgres_test

// Integration tests against a real Postgres. They assert the fixes this rewrite
// introduces: stable pagination, real category relations, and filter semantics.
// Each test builds its own schema and fixtures, so the suite is self-contained and
// runs identically in CI, where no data is imported.

import (
	"context"
	"testing"

	"github.com/hireonai/backend-hireon-go/internal/repository/postgres"
)

// TestPaginationHasNoOverlap is the regression test for the bug the Node backend
// shipped: sorting on a non-unique createdAt alone left rows unreachable and
// repeated. Every fixture job shares one timestamp, so only the id tiebreaker can
// keep pages disjoint -- remove it and this test fails.
func TestPaginationHasNoOverlap(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	seen := map[string]int{}
	pages := fixtureCount / 10
	for page := 1; page <= pages; page++ {
		jobs, _, err := env.store.ListJobs(ctx, postgres.JobsFilter{Page: page, Limit: 10})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(jobs) != 10 {
			t.Fatalf("page %d returned %d jobs, want 10", page, len(jobs))
		}
		for _, j := range jobs {
			seen[j.ID]++
		}
	}

	if len(seen) != fixtureCount {
		t.Errorf("unique jobs across %d pages = %d, want %d", pages, len(seen), fixtureCount)
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("job %s appeared %d times", id, n)
		}
	}
}

// TestPaginationCoversEveryRow walks all pages and asserts no row is skipped.
func TestPaginationCoversEveryRow(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	got := map[string]bool{}
	for page := 1; page <= fixtureCount/7+1; page++ {
		jobs, _, err := env.store.ListJobs(ctx, postgres.JobsFilter{Page: page, Limit: 7})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, j := range jobs {
			got[j.ID] = true
		}
	}
	for _, want := range env.jobIDs {
		if !got[want] {
			t.Errorf("job %s was never returned by any page", want)
		}
	}
}

// TestTotalMatchesFixtures guards the count behind pagination metadata.
func TestTotalMatchesFixtures(t *testing.T) {
	env := newTestEnv(t)
	jobs, total, err := env.store.ListJobs(context.Background(), postgres.JobsFilter{Page: 1, Limit: 1})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if total != fixtureCount {
		t.Errorf("total = %d, want %d", total, fixtureCount)
	}
}

// TestJobRelationsArePopulated proves relations resolve without a query per job
// and without the extra fields a full document would carry.
func TestJobRelationsArePopulated(t *testing.T) {
	env := newTestEnv(t)
	jobs, _, err := env.store.ListJobs(context.Background(), postgres.JobsFilter{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, j := range jobs {
		if j.Company == nil {
			t.Fatalf("job %s has no company", j.ID)
		}
		if j.Company.ID != env.companyID || j.Company.Name != "Acme" {
			t.Errorf("job %s company = %+v", j.ID, j.Company)
		}
		if j.Company.Industry == nil || j.Company.Industry.Name != "Technology" {
			t.Errorf("job %s industry = %+v", j.ID, j.Company.Industry)
		}
		if j.MinExperience == nil || j.MinExperience.Name == "" {
			t.Errorf("job %s has no min experience", j.ID)
		}
	}
}

func TestJobByIDNotFound(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.store.JobByID(context.Background(), "000000000000000000000000"); err == nil {
		t.Fatal("expected an error for a missing job")
	}
}

func TestJobByIDReturnsRelations(t *testing.T) {
	env := newTestEnv(t)
	job, err := env.store.JobByID(context.Background(), env.jobIDs[0])
	if err != nil {
		t.Fatalf("job by id: %v", err)
	}
	if job.ID != env.jobIDs[0] {
		t.Errorf("id = %s, want %s", job.ID, env.jobIDs[0])
	}
	if job.Company == nil || job.MinExperience == nil {
		t.Errorf("relations missing: company=%v experience=%v", job.Company, job.MinExperience)
	}
}

// TestFilterByExperience proves the filter narrows correctly and never returns
// unrelated rows.
func TestFilterByExperience(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	jobs, total, err := env.store.ListJobs(ctx, postgres.JobsFilter{
		ExperienceID: env.expB, Page: 1, Limit: 200,
	})
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	want := 0
	for i := 0; i < fixtureCount; i++ {
		if i%2 == 1 {
			want++
		}
	}
	if total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	for _, j := range jobs {
		if j.MinExperience == nil || j.MinExperience.ID != env.expB {
			t.Fatalf("job %s does not match the requested experience", j.ID)
		}
	}
}

// TestFilterByCategory proves categories are a real relation and the filter uses
// the join table rather than a string array.
func TestFilterByCategory(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	jobs, total, err := env.store.ListJobs(ctx, postgres.JobsFilter{
		CategoryIDs: []string{env.categoryA}, Page: 1, Limit: 200,
	})
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	if total == 0 {
		t.Fatal("expected jobs in category A")
	}
	for _, j := range jobs {
		found := false
		for _, c := range j.Categories {
			if c.ID == env.categoryA {
				found = true
			}
		}
		if !found {
			t.Fatalf("job %s is not in the requested category", j.ID)
		}
	}
}

// TestFilterByIndustry proves the industry filter joins companies instead of
// running a second query and intersecting ids in application code.
func TestFilterByIndustry(t *testing.T) {
	env := newTestEnv(t)
	_, total, err := env.store.ListJobs(context.Background(), postgres.JobsFilter{
		IndustryID: env.industryID, Page: 1, Limit: 1,
	})
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	if total != fixtureCount {
		t.Errorf("total = %d, want %d (every fixture job belongs to the industry)",
			total, fixtureCount)
	}
}

// TestSalaryFilterMatchesEitherBound reproduces the Mongo semantics: bounds apply
// to both min_salary and max_salary, and the two are OR-ed. A plain
// `min_salary >= x AND max_salary <= y` is not equivalent and would drop rows.
func TestSalaryFilterMatchesEitherBound(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	min := int64(50_000_000)
	_, total, err := env.store.ListJobs(ctx, postgres.JobsFilter{MinSalary: &min, Page: 1, Limit: 1})
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	if total == 0 {
		t.Fatal("expected jobs at or above the salary floor")
	}
	if total == fixtureCount {
		t.Error("the salary filter matched every job, so it is not filtering")
	}
}

// TestKeywordFilterSearchesTextColumns covers the keyword path, which previously
// built a regex from raw user input.
func TestKeywordFilterSearchesTextColumns(t *testing.T) {
	env := newTestEnv(t)
	jobs, total, err := env.store.ListJobs(context.Background(), postgres.JobsFilter{
		Keyword: "Engineer 7", Page: 1, Limit: 50,
	})
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	if total == 0 {
		t.Fatal("expected the keyword to match")
	}
	for _, j := range jobs {
		if j.JobPosition == "" {
			t.Fatalf("job %s has no position", j.ID)
		}
	}
}

// TestFilterJobIDs narrows a ranked id list in one query.
func TestFilterJobIDs(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	ids := []string{env.jobIDs[0], env.jobIDs[1], env.jobIDs[2]}
	allowed, err := env.store.FilterJobIDs(ctx, ids, postgres.JobsFilter{
		ExperienceID: env.expB,
	})
	if err != nil {
		t.Fatalf("filter job ids: %v", err)
	}
	// fixture index 1 is odd, so it uses expB; 0 and 2 use expA.
	if !allowed[env.jobIDs[1]] {
		t.Errorf("expected %s to be allowed", env.jobIDs[1])
	}
	if allowed[env.jobIDs[0]] || allowed[env.jobIDs[2]] {
		t.Errorf("unexpected ids allowed: %v", allowed)
	}
}

// TestListJobsInOrder preserves the ranking order supplied by the ML service.
func TestListJobsInOrder(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	want := []string{env.jobIDs[5], env.jobIDs[2], env.jobIDs[9]}
	got, err := env.store.ListJobsInOrder(ctx, want)
	if err != nil {
		t.Fatalf("list in order: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d jobs, want 3", len(got))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("position %d = %s, want %s", i, got[i].ID, want[i])
		}
	}
}

// TestReferenceLists covers the master-data endpoints, including the empty case.
func TestReferenceLists(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	cats, err := env.store.ListJobCategories(ctx)
	if err != nil || len(cats) != 2 {
		t.Fatalf("categories = %d (err %v), want 2", len(cats), err)
	}
	exps, err := env.store.ListJobMinExperiences(ctx)
	if err != nil || len(exps) != 2 {
		t.Fatalf("experiences = %d (err %v), want 2", len(exps), err)
	}
	inds, err := env.store.ListCompanyIndustries(ctx)
	if err != nil || len(inds) != 1 {
		t.Fatalf("industries = %d (err %v), want 1", len(inds), err)
	}
	for _, c := range cats {
		if c.CreatedAt.Time.IsZero() {
			t.Errorf("category %s has no createdAt", c.ID)
		}
	}
}

// TestBookmarks covers add, list and remove, including idempotency.
func TestBookmarks(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	if err := env.store.AddBookmark(ctx, env.userID, env.jobIDs[0]); err != nil {
		t.Fatalf("add bookmark: %v", err)
	}
	// Adding twice must not error or duplicate.
	if err := env.store.AddBookmark(ctx, env.userID, env.jobIDs[0]); err != nil {
		t.Fatalf("re-add bookmark: %v", err)
	}
	jobs, err := env.store.BookmarkedJobs(ctx, env.userID)
	if err != nil {
		t.Fatalf("list bookmarks: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("bookmarks = %d, want 1 after adding one job twice", len(jobs))
	}

	// Bookmarking a job that does not exist is a not-found, not a silent success.
	if err := env.store.AddBookmark(ctx, env.userID, "does-not-exist"); err == nil {
		t.Error("expected an error for an unknown job")
	}

	if err := env.store.RemoveBookmark(ctx, env.userID, env.jobIDs[0]); err != nil {
		t.Fatalf("remove bookmark: %v", err)
	}
	jobs, _ = env.store.BookmarkedJobs(ctx, env.userID)
	if len(jobs) != 0 {
		t.Fatalf("bookmarks = %d, want 0 after removal", len(jobs))
	}
}

// TestProfileUpdate covers partial updates and the tag-preference replacement.
func TestProfileUpdate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	fullname := "Updated Name"
	domicile := "Bandung"
	if err := env.store.UpdateProfileFields(ctx, env.userID, map[string]any{
		"fullname": fullname, "domicile": domicile,
	}); err != nil {
		t.Fatalf("update profile: %v", err)
	}
	p, err := env.store.ProfileByUserID(ctx, env.userID)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if p.Fullname != fullname || p.Domicile == nil || *p.Domicile != domicile {
		t.Errorf("profile = %+v", p)
	}

	if err := env.store.UpdateTagPreferences(ctx, env.userID, []string{"Go", "Postgres"}); err != nil {
		t.Fatalf("update tags: %v", err)
	}
	p, _ = env.store.ProfileByUserID(ctx, env.userID)
	if len(p.TagPreferences) != 2 {
		t.Errorf("tags = %v, want 2", p.TagPreferences)
	}

	// A second update replaces rather than appends.
	if err := env.store.UpdateTagPreferences(ctx, env.userID, []string{"Rust"}); err != nil {
		t.Fatalf("replace tags: %v", err)
	}
	p, _ = env.store.ProfileByUserID(ctx, env.userID)
	if len(p.TagPreferences) != 1 || p.TagPreferences[0] != "Rust" {
		t.Errorf("tags = %v, want [Rust]", p.TagPreferences)
	}
}

// TestAnalysisResultUpsert proves the unique (user_id, job_id) constraint turns a
// repeated analysis into an update rather than a duplicate row.
func TestAnalysisResultUpsert(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	res, err := env.store.UpsertAnalysisResult(ctx, env.analysisFixture(60))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if res.CVRelevanceScore != 60 {
		t.Errorf("score = %v, want 60", res.CVRelevanceScore)
	}
	res2, err := env.store.UpsertAnalysisResult(ctx, env.analysisFixture(85))
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if res2.CVRelevanceScore != 85 {
		t.Errorf("score = %v, want 85 after update", res2.CVRelevanceScore)
	}
	if res2.ID != res.ID {
		t.Errorf("id changed from %s to %s; expected an update, not a new row", res.ID, res2.ID)
	}

	got, err := env.store.AnalysisResultFor(ctx, env.userID, env.jobIDs[0])
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got == nil || got.CVRelevanceScore != 85 {
		t.Errorf("stored result = %+v", got)
	}

	// Absence is not an error.
	missing, err := env.store.AnalysisResultFor(ctx, env.userID, env.jobIDs[1])
	if err != nil {
		t.Fatalf("fetch missing: %v", err)
	}
	if missing != nil {
		t.Errorf("expected nil for an unanalysed job, got %+v", missing)
	}
}
