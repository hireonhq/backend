package postgres_test

// Test infrastructure: every integration test runs inside its own Postgres schema
// with the real DDL applied and its own fixtures.
//
// The earlier version of these tests read whatever happened to be in the database.
// That passed locally against a migrated copy of production but failed in CI,
// where only the schema exists -- and it made the suite's meaning depend on
// ambient data. Fixtures make each assertion self-contained.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hireonai/backend-hireon-go/internal/domain"
	"github.com/hireonai/backend-hireon-go/internal/repository/postgres"
)

// fixtureCount must exceed 100 so the ten-page pagination test exercises real
// paging rather than one short page.
const fixtureCount = 120

// fixtureTime is shared by every seeded job on purpose. The production data had
// only 101 distinct timestamps across 2697 rows, which is exactly the condition
// that made `ORDER BY createdAt` alone skip and repeat rows while paging.
var fixtureTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type testEnv struct {
	store      *postgres.Store
	pool       *pgxpool.Pool
	schema     string
	industryID string
	companyID  string
	categoryA  string
	categoryB  string
	expA       string
	expB       string
	userID     string
	profileID  string
	jobIDs     []string
}

// newTestEnv creates a schema, applies migrations/0001_init.sql into it, and seeds
// fixtures. The schema is dropped on cleanup, so the surrounding database is never
// modified and tests can run in parallel against one server.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	ctx := context.Background()
	schema := "test_" + randomSuffix()

	// The pool uses the default extended protocol, exactly as production does. The
	// simple protocol cannot encode maps or byte slices for typed columns, so using
	// it here would mask real encoding bugs -- and would have hidden the very
	// failures these tests exist to catch.
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	cfg.MaxConns = 4

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})

	ddl, err := os.ReadFile("../../../migrations/0001_init.up.sql")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	// Apply the DDL over a dedicated connection in simple mode, because the file is
	// multi-statement. The pool above stays on the extended protocol.
	applySchema(t, dsn, schema, string(ddl))

	env := &testEnv{store: postgres.New(pool), pool: pool, schema: schema}
	env.seed(t)
	return env
}

// applySchema runs the migration file against a schema using a short-lived
// connection. Simple mode is required because Exec sends several statements at once.
func applySchema(t *testing.T, dsn, schema, ddl string) {
	t.Helper()
	ctx := context.Background()

	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	connCfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	connCfg.RuntimeParams["search_path"] = schema + ",public"

	conn, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		t.Fatalf("connect for ddl: %v", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
}

func (e *testEnv) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed failed: %v", err)
		}
	}

	e.industryID = "ind1"
	e.companyID = "cmp1"
	e.categoryA = "catA"
	e.categoryB = "catB"
	e.expA = "expA"
	e.expB = "expB"
	e.userID = "user1"
	e.profileID = "prof1"

	exec(`INSERT INTO company_industries (id, name) VALUES ($1,$2)`, e.industryID, "Technology")
	exec(`INSERT INTO companies (id, name, description, profile_src, location, employees_count, industry_id)
	      VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		e.companyID, "Acme", "desc", "profile.png", "Jakarta", "10-50", e.industryID)
	exec(`INSERT INTO job_categories (id, name) VALUES ($1,$2), ($3,$4)`,
		e.categoryA, "Engineering", e.categoryB, "Design")
	exec(`INSERT INTO job_min_experiences (id, name) VALUES ($1,$2), ($3,$4)`,
		e.expA, "Min. 1 years of experience", e.expB, "Min. 3 years of experience")
	exec(`INSERT INTO users (id, username, email, supabase_id, role)
	      VALUES ($1,$2,$3,$4,'seeker')`, e.userID, "testuser", "test@example.test", "sub-1")
	exec(`INSERT INTO profiles (id, user_id, fullname) VALUES ($1,$2,$3)`,
		e.profileID, e.userID, "Test User")

	// All jobs share fixtureTime, reproducing the tie that broke pagination. Ids are
	// zero-padded so id DESC is a well-defined tiebreaker.
	for i := 0; i < fixtureCount; i++ {
		id := fmt.Sprintf("job%03d", i)
		exp := e.expA
		if i%2 == 1 {
			exp = e.expB
		}
		exec(`INSERT INTO jobs (id, url, company_id, job_position, employment_type,
		        working_location_type, working_location, min_experience_id,
		        min_salary, max_salary, job_desc_list, job_qualifications_list, created_at, updated_at)
		      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13)`,
			id,
			fmt.Sprintf("https://example.test/jobs/%d", i),
			e.companyID,
			fmt.Sprintf("Engineer %d", i),
			"Full time",
			"On-site",
			"Jakarta",
			exp,
			int64(i)*1_000_000,
			int64(i)*1_000_000+5_000_000,
			[]string{"desc one", "desc two"},
			[]string{"qual one"},
			fixtureTime)
		e.jobIDs = append(e.jobIDs, id)

		switch i % 3 {
		case 0:
			exec(`INSERT INTO job_category_links (job_id, category_id, position) VALUES ($1,$2,0)`, id, e.categoryA)
		case 1:
			exec(`INSERT INTO job_category_links (job_id, category_id, position) VALUES ($1,$2,0)`, id, e.categoryB)
		}
	}
}

// analysisFixture builds an analysis result for the first seeded job.
func (e *testEnv) analysisFixture(score float64) *domain.AnalysisResult {
	return &domain.AnalysisResult{
		UserID:                 e.userID,
		JobID:                  e.jobIDs[0],
		CVRelevanceScore:       score,
		Explanation:            "because",
		SkilIdentificationDict: map[string]any{"Go": 90},
		Suggestions:            []string{"add metrics"},
		Improvements:           []string{"quantify impact"},
	}
}

// queueEnv is the fixture surface the queue tests need.
func (e *testEnv) queue() *postgres.Queue { return e.store.Queue() }

func randomSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// simulateStale backdates a running job to mimic a process that died mid-job.
func (e *testEnv) simulateStale(t *testing.T, id string, age time.Duration) {
	t.Helper()
	_, err := e.pool.Exec(context.Background(),
		`UPDATE jobs_queue SET status='running', updated_at = now() - $2::interval WHERE id = $1`,
		id, age.String())
	if err != nil {
		t.Fatalf("simulate stale: %v", err)
	}
}
