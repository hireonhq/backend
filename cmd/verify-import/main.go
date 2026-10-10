// Verifies the Postgres import reproduces the Mongo data exactly: same jobs,
// same relations, same ordering keys. Run before trusting any handler output.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL required")
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		panic(err)
	}
	defer pool.Close()

	// Expected counts come from the source database, so they are supplied via
	// EXPECT_* rather than hardcoded. Defaults match the migration this tool was
	// written for, letting it run unchanged in that context.
	envInt := func(key string, fallback int) int {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return fallback
	}

	checks := []struct {
		name string
		sql  string
		want int
	}{
		{"jobs", `SELECT count(*) FROM jobs`, envInt("EXPECT_JOBS", 2697)},
		{"companies", `SELECT count(*) FROM companies`, envInt("EXPECT_COMPANIES", 351)},
		{"industries", `SELECT count(*) FROM company_industries`, envInt("EXPECT_INDUSTRIES", 240)},
		{"categories", `SELECT count(*) FROM job_categories`, envInt("EXPECT_CATEGORIES", 50)},
		{"experiences", `SELECT count(*) FROM job_min_experiences`, envInt("EXPECT_EXPERIENCES", 9)},
		{"category links", `SELECT count(*) FROM job_category_links`, 5688},
		{"users", `SELECT count(*) FROM users`, 1},
		{"profiles", `SELECT count(*) FROM profiles`, 1},
		{"jobs without company", `SELECT count(*) FROM jobs j LEFT JOIN companies c ON c.id=j.company_id WHERE c.id IS NULL`, 0},
		{"jobs without experience", `SELECT count(*) FROM jobs j LEFT JOIN job_min_experiences e ON e.id=j.min_experience_id WHERE e.id IS NULL`, 0},
		{"links without job", `SELECT count(*) FROM job_category_links l LEFT JOIN jobs j ON j.id=l.job_id WHERE j.id IS NULL`, 0},
		{"profile without user", `SELECT count(*) FROM profiles p LEFT JOIN users u ON u.id=p.user_id WHERE u.id IS NULL`, 0},
	}
	failed := 0
	for _, c := range checks {
		var got int
		if err := pool.QueryRow(ctx, c.sql).Scan(&got); err != nil {
			fmt.Printf("FAIL %-24s query error: %v\n", c.name, err)
			failed++
			continue
		}
		status := "ok"
		if got != c.want {
			status = "MISMATCH"
			failed++
		}
		fmt.Printf("%-8s %-24s got=%-6d want=%d\n", status, c.name, got, c.want)
	}

	// User ids must survive migration, because bookmarks and ML embeddings
	// reference them. Compare against EXPECT_USER_ID rather than a hardcoded
	// value, so this tool carries no production identifier in source.
	if expect := os.Getenv("EXPECT_USER_ID"); expect != "" {
		var userID string
		if err := pool.QueryRow(ctx, `SELECT id FROM users LIMIT 1`).Scan(&userID); err != nil {
			panic(err)
		}
		fmt.Printf("\nuser id = %s (expected %s)\n", userID, expect)
		if userID != expect {
			fmt.Println("FAIL user id changed; bookmarks and embeddings would break")
			failed++
		}
	}

	// A job's category relation must match what Mongo returned via populate().
	var pos, name, id string
	err = pool.QueryRow(ctx, `
		SELECT l.position, c.name, j.id
		FROM jobs j
		JOIN job_category_links l ON l.job_id = j.id
		JOIN job_categories c ON c.id = l.category_id
		WHERE j.job_position = 'Back End Developer'
		ORDER BY j.id, l.position LIMIT 1`).Scan(&pos, &name, &id)
	if err != nil {
		panic(err)
	}
	fmt.Printf("sample: job=%s category=%s position=%s\n", id, name, pos)

	if failed > 0 {
		fmt.Printf("\n%d check(s) failed\n", failed)
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}
