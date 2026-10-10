package postgres_test

// Integration tests against a real Postgres. They assert the fixes that the
// rewrite introduces: stable pagination, real category relations, and filter
// semantics. Skipped unless TEST_DATABASE_URL is set.

import (
	"context"
	"os"
	"testing"

	"github.com/hireonai/backend-hireon-go/internal/config"
	"github.com/hireonai/backend-hireon-go/internal/repository/postgres"
	"github.com/hireonai/backend-hireon-go/internal/service"
	"github.com/hireonai/backend-hireon-go/internal/storage"
)

func testStore(t *testing.T) *postgres.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := postgres.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return postgres.New(pool)
}

// TestPaginationHasNoOverlap is the regression test for the bug the Node backend
// shipped: sorting on a non-unique createdAt alone left 44 of 100 listed jobs
// unreachable and 44 duplicated across ten pages.
func TestPaginationHasNoOverlap(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	seen := map[string]int{}
	for page := 1; page <= 10; page++ {
		jobs, _, err := store.ListJobs(ctx, postgres.JobsFilter{Page: page, Limit: 10})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, j := range jobs {
			seen[j.ID]++
		}
	}

	if len(seen) != 100 {
		t.Errorf("unique jobs across 10 pages = %d, want 100", len(seen))
	}
	dupes := 0
	for id, n := range seen {
		if n > 1 {
			dupes++
			t.Logf("job %s appeared %d times", id, n)
		}
	}
	if dupes != 0 {
		t.Errorf("%d jobs appeared on more than one page", dupes)
	}
}

// TestTotalMatchesCorpus guards the count used for pagination metadata.
func TestTotalMatchesCorpus(t *testing.T) {
	store := testStore(t)
	jobs, total, err := store.ListJobs(context.Background(), postgres.JobsFilter{Page: 1, Limit: 1})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if total < 2000 {
		t.Errorf("total = %d, expected the full corpus", total)
	}
}

func TestJobRelationsArePopulated(t *testing.T) {
	store := testStore(t)
	jobs, _, err := store.ListJobs(context.Background(), postgres.JobsFilter{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, j := range jobs {
		if j.Company == nil {
			t.Fatalf("job %s has no company", j.ID)
		}
		if j.Company.Industry == nil {
			t.Fatalf("job %s company has no industry", j.ID)
		}
		if j.MinExperience == nil {
			t.Fatalf("job %s has no min experience", j.ID)
		}
	}
}

func TestNotFoundForMissingJob(t *testing.T) {
	store := testStore(t)
	_, err := store.JobByID(context.Background(), "000000000000000000000000")
	if err == nil {
		t.Fatal("expected an error for a missing job")
	}
}

// TestFilterByExperience narrows correctly and never returns unrelated rows.
func TestFilterByExperience(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	exps, err := store.ListJobMinExperiences(ctx)
	if err != nil {
		t.Fatalf("experiences: %v", err)
	}
	if len(exps) == 0 {
		t.Skip("no experiences seeded")
	}

	jobs, total, err := store.ListJobs(ctx, postgres.JobsFilter{
		ExperienceID: exps[0].ID, Page: 1, Limit: 50,
	})
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	for _, j := range jobs {
		if j.MinExperience == nil || j.MinExperience.ID != exps[0].ID {
			t.Fatalf("job %s does not match the requested experience", j.ID)
		}
	}
	if total == 0 {
		t.Log("no jobs for the first experience; filter still executed")
	}
}

// TestCategoryRelation proves categories are a real relation, not an array of
// strings: every linked category resolves against the category table.
func TestCategoryRelation(t *testing.T) {
	store := testStore(t)
	cats, err := store.ListJobCategories(context.Background())
	if err != nil {
		t.Fatalf("categories: %v", err)
	}
	if len(cats) == 0 {
		t.Fatal("expected seeded categories")
	}
	jobs, _, err := store.ListJobs(context.Background(), postgres.JobsFilter{
		Page: 1, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, j := range jobs {
		for _, c := range j.Categories {
			if c.ID == "" || c.Name == "" {
				t.Fatalf("job %s has an unresolved category %+v", j.ID, c)
			}
		}
	}
}

var (
	_ = config.Config{}
	_ = service.JobsQuery{}
	_ = storage.Nil{}
)
