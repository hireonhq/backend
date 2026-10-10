package postgres_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// queuePool gives tests direct SQL access for setup and cleanup that is not part
// of the repository API under test.
func queuePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seededUserID returns a user id that exists, since jobs_queue references users.
func seededUserID(t *testing.T) string {
	t.Helper()
	pool := queuePool(t)
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM users LIMIT 1`).Scan(&id); err != nil {
		t.Skipf("no seeded user available: %v", err)
	}
	return id
}

func cleanupJob(t *testing.T, id string) {
	t.Helper()
	pool := queuePool(t)
	_, _ = pool.Exec(context.Background(), `DELETE FROM jobs_queue WHERE id = $1`, id)
}

// simulateStale backdates a running job to mimic a process that died mid-job.
func simulateStale(t *testing.T, id string, age time.Duration) {
	t.Helper()
	pool := queuePool(t)
	_, err := pool.Exec(context.Background(),
		`UPDATE jobs_queue SET status='running', updated_at = now() - $2::interval WHERE id = $1`,
		id, age.String())
	if err != nil {
		t.Fatalf("simulate stale: %v", err)
	}
}
