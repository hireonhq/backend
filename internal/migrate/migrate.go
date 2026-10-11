// Package migrate applies pending schema migrations.
//
// The schema previously had to be applied by hand with `psql -f`, which is not
// reproducible: nothing recorded which migrations had run, so a deploy could
// silently run against an out-of-date schema and the same SQL could be applied
// twice. Each file is applied once, inside a transaction, and recorded in
// schema_migrations with a checksum so later edits are detected.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Dir is the default location of the SQL files.
const Dir = "migrations"

// Migration is one SQL file plus its optional reversible counterpart.
type Migration struct {
	Version  string
	Name     string
	UpPath   string
	DownPath string
}

// loadMigrations reads `<version>_<name>.up.sql` and an optional `.down.sql`.
func Load(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir %s: %w", dir, err)
	}

	byVersion := map[string]*Migration{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, direction, err := parseFilename(e.Name())
		if err != nil {
			return nil, err
		}
		m, ok := byVersion[version]
		if !ok {
			m = &Migration{Version: version, Name: name}
			byVersion[version] = m
		}
		path := filepath.Join(dir, e.Name())
		switch direction {
		case "up":
			m.UpPath = path
		case "down":
			m.DownPath = path
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.UpPath == "" {
			return nil, fmt.Errorf("migration %s has no .up.sql file", m.Version)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	// Duplicate versions would apply in an arbitrary order.
	// Versions sort lexicographically; duplicate prefixes would apply in an
	// arbitrary order, so they are rejected.
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("duplicate migration version %s", out[i].Version)
		}
	}
	return out, nil
}

// Run applies every pending migration in dir. It is safe to call on every start:
// already-applied migrations are skipped.
func Run(ctx context.Context, conn *pgx.Conn, dir string) error {
	migrations, err := Load(dir)
	if err != nil {
		return err
	}
	if err := ensureTable(ctx, conn); err != nil {
		return fmt.Errorf("prepare schema_migrations: %w", err)
	}
	return Up(ctx, conn, migrations)
}

// Report prints each migration's state.
func Report(ctx context.Context, conn *pgx.Conn, dir string) error {
	migrations, err := Load(dir)
	if err != nil {
		return err
	}
	if err := ensureTable(ctx, conn); err != nil {
		return err
	}
	return Status(ctx, conn, migrations)
}

// Revert reverts the most recent migrations. It requires MIGRATE_ALLOW_DOWN=yes so
// a stray invocation cannot destroy data.
func Revert(ctx context.Context, conn *pgx.Conn, dir string, steps int) error {
	migrations, err := Load(dir)
	if err != nil {
		return err
	}
	if err := ensureTable(ctx, conn); err != nil {
		return err
	}
	return Down(ctx, conn, migrations, steps)
}

func parseFilename(name string) (version, label, direction string, err error) {
	base := strings.TrimSuffix(name, ".sql")
	switch {
	case strings.HasSuffix(base, ".up"):
		direction, base = "up", strings.TrimSuffix(base, ".up")
	case strings.HasSuffix(base, ".down"):
		direction, base = "down", strings.TrimSuffix(base, ".down")
	default:
		return "", "", "", fmt.Errorf("migration %s must end in .up.sql or .down.sql", name)
	}

	idx := strings.Index(base, "_")
	if idx <= 0 {
		return "", "", "", fmt.Errorf("migration %s must be named <version>_<name>.%s.sql", name, direction)
	}
	return base[:idx], base[idx+1:], direction, nil
}

func ensureTable(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    version    text PRIMARY KEY,
		    name       text NOT NULL,
		    checksum   text NOT NULL,
		    applied_at timestamptz NOT NULL DEFAULT now()
		)`)
	return err
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, err
		}
		out[version] = checksum
	}
	return out, rows.Err()
}

func Up(ctx context.Context, conn *pgx.Conn, migrations []Migration) error {
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	ran := 0
	for _, m := range migrations {
		body, err := os.ReadFile(m.UpPath)
		if err != nil {
			return err
		}
		sum := checksum(body)

		if prev, ok := applied[m.Version]; ok {
			// A changed file that is already applied means the database and the
			// repository disagree; refusing is safer than silently diverging.
			if prev != sum {
				return fmt.Errorf(
					"migration %s was already applied with a different checksum\n"+
						"  applied: %s\n  current: %s\n"+
						"Create a new migration instead of editing an applied one",
					m.Version, prev[:12], sum[:12])
			}
			continue
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s_%s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1,$2,$3)`,
			m.Version, m.Name, sum); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		fmt.Printf("applied %s_%s\n", m.Version, m.Name)
		ran++
	}

	if ran == 0 {
		fmt.Println("no pending migrations; database is up to date")
	} else {
		fmt.Printf("applied %d migration(s)\n", ran)
	}
	return nil
}

func Status(ctx context.Context, conn *pgx.Conn, migrations []Migration) error {
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}
	fmt.Printf("%-8s %-34s %s\n", "VERSION", "NAME", "STATE")
	for _, m := range migrations {
		state := "pending"
		if _, ok := applied[m.Version]; ok {
			state = "applied"
		}
		if m.DownPath == "" {
			state += " (irreversible)"
		}
		fmt.Printf("%-8s %-34s %s\n", m.Version, m.Name, state)
	}
	return nil
}

// down reverts the most recent migrations. It requires MIGRATE_ALLOW_DOWN=yes so a
// stray invocation cannot destroy data.
func Down(ctx context.Context, conn *pgx.Conn, migrations []Migration, steps int) error {
	if os.Getenv("MIGRATE_ALLOW_DOWN") != "yes" {
		return errors.New("refusing to revert: set MIGRATE_ALLOW_DOWN=yes to confirm")
	}
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	var toRevert []Migration
	for i := len(migrations) - 1; i >= 0 && len(toRevert) < steps; i-- {
		if _, ok := applied[migrations[i].Version]; ok {
			toRevert = append(toRevert, migrations[i])
		}
	}
	if len(toRevert) == 0 {
		fmt.Println("nothing to revert")
		return nil
	}

	for _, m := range toRevert {
		if m.DownPath == "" {
			return fmt.Errorf("migration %s has no .down.sql; revert it by hand", m.Version)
		}
		body, err := os.ReadFile(m.DownPath)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("revert %s_%s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, m.Version); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		fmt.Printf("reverted %s_%s\n", m.Version, m.Name)
	}
	return nil
}

func checksum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
