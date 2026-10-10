// Command import loads a Mongo dump (see tools/dump-mongo) into Postgres.
//
// Every row keeps its original ObjectId hex `id`, so foreign keys in Postgres
// match the ids already stored in user bookmarks, ML embeddings and the
// frontend. All writes are upserts, so re-running never destroys user data --
// unlike the old Mongo seeder, which called deleteMany() and minted fresh ids on
// every run, orphaning every bookmark and embedding.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
)

type row = map[string]any

func load(dir, name string) ([]row, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		return nil, err
	}
	var out []row
	return out, json.Unmarshal(raw, &out)
}

var str = func(v any) any {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

var num = func(v any) any {
	if v == nil {
		return nil
	}
	return v
}

func ts(v any) any {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return t
}

func strs(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(list))
	for _, it := range list {
		out = append(out, fmt.Sprint(it))
	}
	return out
}

func main() {
	_ = godotenv.Load("../backend-hireonai/.env")
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(1)
	}
	dir := "dump"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	if err := run(ctx, conn, dir); err != nil {
		fmt.Fprintln(os.Stderr, "import failed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, conn *pgx.Conn, dir string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Parent tables first so foreign keys resolve.
	type step struct {
		table  string
		insert string
		build  func(row) []any
	}
	steps := []step{
		{"company_industries",
			`INSERT INTO company_industries (id,name,created_at,updated_at) VALUES ($1,$2,$3,$4)
			 ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, updated_at=EXCLUDED.updated_at`,
			func(r row) []any { return []any{r["id"], r["name"], ts(r["created_at"]), ts(r["updated_at"])} }},
		{"companies",
			`INSERT INTO companies (id,name,description,profile_src,location,employees_count,industry_id,created_at,updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			 ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, description=EXCLUDED.description,
			   profile_src=EXCLUDED.profile_src, location=EXCLUDED.location,
			   employees_count=EXCLUDED.employees_count, industry_id=EXCLUDED.industry_id,
			   updated_at=EXCLUDED.updated_at`,
			func(r row) []any {
				return []any{r["id"], r["name"], r["description"], r["profile_src"], r["location"],
					str(r["employees_count"]), r["industry_id"], ts(r["created_at"]), ts(r["updated_at"])}
			}},
		{"job_categories",
			`INSERT INTO job_categories (id,name,created_at,updated_at) VALUES ($1,$2,$3,$4)
			 ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, updated_at=EXCLUDED.updated_at`,
			func(r row) []any { return []any{r["id"], r["name"], ts(r["created_at"]), ts(r["updated_at"])} }},
		{"job_min_experiences",
			`INSERT INTO job_min_experiences (id,name,created_at,updated_at) VALUES ($1,$2,$3,$4)
			 ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, updated_at=EXCLUDED.updated_at`,
			func(r row) []any { return []any{r["id"], r["name"], ts(r["created_at"]), ts(r["updated_at"])} }},
		{"users",
			`INSERT INTO users (id,username,email,supabase_id,password,photo_url,role,oauth_provider,verified_at,created_at,updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			 ON CONFLICT (id) DO UPDATE SET username=EXCLUDED.username, email=EXCLUDED.email,
			   supabase_id=EXCLUDED.supabase_id, password=EXCLUDED.password, photo_url=EXCLUDED.photo_url,
			   role=EXCLUDED.role, oauth_provider=EXCLUDED.oauth_provider,
			   verified_at=EXCLUDED.verified_at, updated_at=EXCLUDED.updated_at`,
			func(r row) []any {
				return []any{r["id"], r["username"], r["email"], str(r["supabase_id"]), str(r["password"]),
					str(r["photo_url"]), str(r["role"]), str(r["oauth_provider"]), ts(r["verified_at"]),
					ts(r["created_at"]), ts(r["updated_at"])}
			}},
		{"jobs",
			`INSERT INTO jobs (id,url,company_id,job_position,employment_type,working_location_type,
			   working_location,min_experience_id,min_salary,max_salary,job_desc_list,
			   job_qualifications_list,created_at,updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			 ON CONFLICT (id) DO UPDATE SET url=EXCLUDED.url, company_id=EXCLUDED.company_id,
			   job_position=EXCLUDED.job_position, employment_type=EXCLUDED.employment_type,
			   working_location_type=EXCLUDED.working_location_type,
			   working_location=EXCLUDED.working_location, min_experience_id=EXCLUDED.min_experience_id,
			   min_salary=EXCLUDED.min_salary, max_salary=EXCLUDED.max_salary,
			   job_desc_list=EXCLUDED.job_desc_list,
			   job_qualifications_list=EXCLUDED.job_qualifications_list, updated_at=EXCLUDED.updated_at`,
			func(r row) []any {
				return []any{r["id"], r["url"], r["company_id"], r["job_position"], r["employment_type"],
					r["working_location_type"], str(r["working_location"]), r["min_experience_id"],
					num(r["min_salary"]), num(r["max_salary"]), strs(r["job_desc_list"]),
					strs(r["job_qualifications_list"]), ts(r["created_at"]), ts(r["updated_at"])}
			}},
		{"profiles",
			`INSERT INTO profiles (id,user_id,fullname,phone,domicile,last_education,photo_url,
			   portfolio_url,cv_url,cv_status,tag_preferences,created_at,updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			 ON CONFLICT (id) DO UPDATE SET user_id=EXCLUDED.user_id, fullname=EXCLUDED.fullname,
			   phone=EXCLUDED.phone, domicile=EXCLUDED.domicile, last_education=EXCLUDED.last_education,
			   photo_url=EXCLUDED.photo_url, portfolio_url=EXCLUDED.portfolio_url,
			   cv_url=EXCLUDED.cv_url, cv_status=EXCLUDED.cv_status,
			   tag_preferences=EXCLUDED.tag_preferences, updated_at=EXCLUDED.updated_at`,
			func(r row) []any {
				return []any{r["id"], r["user_id"], r["fullname"], str(r["phone"]), str(r["domicile"]),
					str(r["last_education"]), str(r["photo_url"]), str(r["portfolio_url"]),
					str(r["cv_url"]), str(r["cv_status"]), strs(r["tag_preferences"]),
					ts(r["created_at"]), ts(r["updated_at"])}
			}},
	}

	for _, s := range steps {
		rows, err := load(dir, s.table)
		if err != nil {
			return fmt.Errorf("load %s: %w", s.table, err)
		}
		for _, r := range rows {
			if _, err := tx.Exec(ctx, s.insert, s.build(r)...); err != nil {
				return fmt.Errorf("%s id=%v: %w", s.table, r["id"], err)
			}
		}
		fmt.Printf("%-22s %d\n", s.table, len(rows))
	}

	// Child tables: replace rows keyed by the parent so re-running stays clean.
	links, err := load(dir, "job_category_links")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM job_category_links`); err != nil {
		return err
	}
	for _, r := range links {
		if _, err := tx.Exec(ctx,
			`INSERT INTO job_category_links (job_id, category_id, position) VALUES ($1,$2,$3)
			 ON CONFLICT DO NOTHING`,
			r["job_id"], r["category_id"], num(r["position"])); err != nil {
			return fmt.Errorf("job_category_links: %w", err)
		}
	}
	fmt.Printf("%-22s %d\n", "job_category_links", len(links))

	bookmarks, err := load(dir, "bookmark_jobs")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM bookmark_jobs`); err != nil {
		return err
	}
	for _, r := range bookmarks {
		if _, err := tx.Exec(ctx,
			`INSERT INTO bookmark_jobs (profile_id, job_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			r["profile_id"], r["job_id"]); err != nil {
			return fmt.Errorf("bookmark_jobs: %w", err)
		}
	}
	fmt.Printf("%-22s %d\n", "bookmark_jobs", len(bookmarks))

	analyses, err := load(dir, "analysis_results")
	if err != nil {
		return err
	}
	for _, r := range analyses {
		skills, _ := json.Marshal(r["skil_identification_dict"])
		if _, err := tx.Exec(ctx,
			`INSERT INTO analysis_results (id,user_id,job_id,cv_relevance_score,explanation,
			   skil_identification_dict,suggestions,improvements,created_at,updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			 ON CONFLICT (id) DO UPDATE SET cv_relevance_score=EXCLUDED.cv_relevance_score,
			   explanation=EXCLUDED.explanation, skil_identification_dict=EXCLUDED.skil_identification_dict,
			   suggestions=EXCLUDED.suggestions, improvements=EXCLUDED.improvements,
			   updated_at=EXCLUDED.updated_at`,
			r["id"], r["user_id"], r["job_id"], num(r["cv_relevance_score"]), r["explanation"],
			string(skills), strs(r["suggestions"]), strs(r["improvements"]),
			ts(r["created_at"]), ts(r["updated_at"])); err != nil {
			return fmt.Errorf("analysis_results: %w", err)
		}
	}
	fmt.Printf("%-22s %d\n", "analysis_results", len(analyses))

	return tx.Commit(ctx)
}
