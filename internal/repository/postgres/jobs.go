// Package postgres implements the repository interfaces against Postgres.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hireonai/backend-hireon-go/internal/domain"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Queue returns the work queue backed by the same connection pool.
func (s *Store) Queue() *Queue { return &Queue{pool: s.pool} }

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// JobsFilter mirrors the query parameters accepted by GET /jobs.
type JobsFilter struct {
	Keyword      string
	MinSalary    *int64
	MaxSalary    *int64
	ExperienceID string
	CategoryIDs  []string
	IndustryID   string
	Page         int
	Limit        int

	// PreferenceTags/CategoryIDs are set only for the tag-preference listing mode.
	PreferenceTags        []string
	PreferenceCategoryIDs []string
}

// buildWhere assembles the filter clause and its arguments.
//
// Categories are filtered through the join table rather than a string array, and
// the industry filter joins companies instead of issuing a second query and
// passing an id list back in -- the old implementation ran Company.find() and
// then matched ids in application code.
func (f JobsFilter) buildWhere() (string, []any) {
	var clauses []string
	var args []any
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}

	if f.Keyword != "" {
		args = append(args, f.Keyword)
		n := len(args)
		clauses = append(clauses, fmt.Sprintf(
			`(j.job_position ILIKE '%%'||$%d||'%%'
			  OR array_to_string(j.job_desc_list, ' ') ILIKE '%%'||$%d||'%%'
			  OR array_to_string(j.job_qualifications_list, ' ') ILIKE '%%'||$%d||'%%'
			  OR coalesce(j.working_location,'') ILIKE '%%'||$%d||'%%')`, n, n, n, n))
	}
	// Salary filtering reproduces the Mongo semantics exactly: the bounds are
	// applied to BOTH min_salary and max_salary, and the two fields are OR-ed.
	// A plain `min_salary >= x AND max_salary <= y` is not equivalent -- it drops
	// every job whose range straddles the requested band.
	if f.MinSalary != nil || f.MaxSalary != nil {
		bound := func(col string) string {
			var parts []string
			if f.MinSalary != nil {
				args = append(args, *f.MinSalary)
				parts = append(parts, fmt.Sprintf("%s >= $%d", col, len(args)))
			}
			if f.MaxSalary != nil {
				args = append(args, *f.MaxSalary)
				parts = append(parts, fmt.Sprintf("%s <= $%d", col, len(args)))
			}
			return "(" + strings.Join(parts, " AND ") + ")"
		}
		clauses = append(clauses, "("+bound("j.min_salary")+" OR "+bound("j.max_salary")+")")
	}
	if f.ExperienceID != "" {
		add(`j.min_experience_id = $%d`, f.ExperienceID)
	}
	if f.IndustryID != "" {
		add(`c.industry_id = $%d`, f.IndustryID)
	}
	if len(f.CategoryIDs) > 0 {
		add(`EXISTS (SELECT 1 FROM job_category_links l
		             WHERE l.job_id = j.id AND l.category_id = ANY($%d))`, f.CategoryIDs)
	}
	if len(f.PreferenceTags) > 0 {
		args = append(args, f.PreferenceTags)
		n := len(args)
		var parts []string
		for _, col := range []string{"j.job_position", "j.employment_type",
			"j.working_location_type", "coalesce(j.working_location,'')"} {
			parts = append(parts, fmt.Sprintf(`%s ILIKE ANY($%d)`, col, n))
		}
		parts = append(parts,
			fmt.Sprintf(`array_to_string(j.job_desc_list, ' ') ILIKE ANY($%d)`, n),
			fmt.Sprintf(`array_to_string(j.job_qualifications_list, ' ') ILIKE ANY($%d)`, n))
		if len(f.PreferenceCategoryIDs) > 0 {
			args = append(args, f.PreferenceCategoryIDs)
			m := len(args)
			parts = append(parts, fmt.Sprintf(
				`EXISTS (SELECT 1 FROM job_category_links l2
				         WHERE l2.job_id = j.id AND l2.category_id = ANY($%d))`, m))
		}
		clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// ListJobs returns a page of jobs plus the total matching count.
//
// Ordering is `created_at DESC, id DESC`. The Node backend sorted on createdAt
// alone, and because only 101 distinct timestamps exist across 2697 rows, most
// rows tie: pages overlapped and the same job could appear twice or never. The id
// tiebreaker makes pagination stable.
func (s *Store) ListJobs(ctx context.Context, f JobsFilter) ([]domain.Job, int, error) {
	where, args := f.buildWhere()

	var total int
	countSQL := `SELECT count(*) FROM jobs j JOIN companies c ON c.id = j.company_id` + where
	if err := s.pool.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count jobs: %w", err)
	}

	args = append(args, f.Limit, (f.Page-1)*f.Limit)
	sql := fmt.Sprintf(`
		SELECT j.id, j.url, j.company_id, j.job_position, j.employment_type,
		       j.working_location_type, j.working_location, j.min_experience_id,
		       j.min_salary, j.max_salary, j.job_desc_list, j.job_qualifications_list,
		       j.created_at, j.updated_at
		FROM jobs j JOIN companies c ON c.id = j.company_id%s
		ORDER BY j.created_at DESC, j.id DESC
		LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	jobs, err := scanJobs(rows)
	if err != nil {
		return nil, 0, err
	}
	if len(jobs) == 0 {
		return jobs, total, nil
	}

	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	if err := s.attachJobRelations(ctx, jobs, ids); err != nil {
		return nil, 0, err
	}
	return jobs, total, nil
}

// ListJobsInOrder returns the given job ids in the order supplied, preserving
// ranking from the recommendation service.
func (s *Store) ListJobsInOrder(ctx context.Context, ids []string) ([]domain.Job, error) {
	if len(ids) == 0 {
		return []domain.Job{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT j.id, j.url, j.company_id, j.job_position, j.employment_type,
		       j.working_location_type, j.working_location, j.min_experience_id,
		       j.min_salary, j.max_salary, j.job_desc_list, j.job_qualifications_list,
		       j.created_at, j.updated_at
		FROM jobs j WHERE j.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("list jobs by id: %w", err)
	}
	defer rows.Close()

	jobs, err := scanJobs(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachJobRelations(ctx, jobs, ids); err != nil {
		return nil, err
	}

	byID := make(map[string]domain.Job, len(jobs))
	for _, j := range jobs {
		byID[j.ID] = j
	}
	ordered := make([]domain.Job, 0, len(ids))
	for _, id := range ids {
		if j, ok := byID[id]; ok {
			ordered = append(ordered, j)
		}
	}
	return ordered, nil
}

// FilterJobIDs returns the subset of ids matching a filter, so recommendation
// results can be narrowed in one query instead of fetching every recommended id
// and intersecting in application code.
func (s *Store) FilterJobIDs(ctx context.Context, ids []string, f JobsFilter) (map[string]bool, error) {
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	where, args := f.buildWhere()
	args = append([]any{ids}, args...)
	// The id list is $1; shift the filter placeholders built by buildWhere.
	if where != "" {
		for i := 2; i <= len(args); i++ {
			where = strings.ReplaceAll(where, fmt.Sprintf("$%d", i-1), fmt.Sprintf("$%d", i))
		}
	}
	sql := `SELECT j.id FROM jobs j JOIN companies c ON c.id = j.company_id
	        WHERE j.id = ANY($1)` + strings.Replace(where, " WHERE ", " AND ", 1)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("filter job ids: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func scanJobs(rows pgx.Rows) ([]domain.Job, error) {
	var out []domain.Job
	for rows.Next() {
		var j domain.Job
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&j.ID, &j.URL, new(string), &j.JobPosition, &j.EmploymentType,
			&j.WorkingLocationType, &j.WorkingLocation, new(string),
			&j.MinSalary, &j.MaxSalary, &j.JobDescList, &j.JobQualificationsList,
			&createdAt, &updatedAt); err != nil {
			return nil, err
		}
		j.CreatedAt = domain.NewMongoTime(createdAt)
		j.UpdatedAt = domain.NewMongoTime(updatedAt)
		out = append(out, j)
	}
	return out, rows.Err()
}

// attachJobRelations loads categories, companies and experiences for a batch of
// jobs with one query per relation, avoiding a query per job.
func (s *Store) attachJobRelations(ctx context.Context, jobs []domain.Job, ids []string) error {
	cats, err := s.categoriesFor(ctx, ids)
	if err != nil {
		return err
	}
	companies, err := s.companiesForJobs(ctx, ids)
	if err != nil {
		return err
	}
	exp, err := s.experiencesForJobs(ctx, ids)
	if err != nil {
		return err
	}
	for i := range jobs {
		jobs[i].Categories = cats[jobs[i].ID]
		if jobs[i].Categories == nil {
			jobs[i].Categories = []domain.Ref{}
		}
		if c, ok := companies[jobs[i].ID]; ok {
			jobs[i].Company = c
		}
		if e, ok := exp[jobs[i].ID]; ok {
			jobs[i].MinExperience = e
		}
	}
	return nil
}

func (s *Store) categoriesFor(ctx context.Context, ids []string) (map[string][]domain.Ref, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT l.job_id, c.id, c.name
		FROM job_category_links l JOIN job_categories c ON c.id = l.category_id
		WHERE l.job_id = ANY($1)
		ORDER BY l.job_id, l.position`, ids)
	if err != nil {
		return nil, fmt.Errorf("categories: %w", err)
	}
	defer rows.Close()

	out := map[string][]domain.Ref{}
	for rows.Next() {
		var jobID string
		var c domain.Ref
		if err := rows.Scan(&jobID, &c.ID, &c.Name); err != nil {
			return nil, err
		}
		out[jobID] = append(out[jobID], c)
	}
	return out, rows.Err()
}

func (s *Store) companiesForJobs(ctx context.Context, ids []string) (map[string]*domain.CompanyRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT j.id, c.id, c.name, i.id, i.name
		FROM jobs j
		JOIN companies c ON c.id = j.company_id
		JOIN company_industries i ON i.id = c.industry_id
		WHERE j.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("companies: %w", err)
	}
	defer rows.Close()

	out := map[string]*domain.CompanyRef{}
	for rows.Next() {
		var jobID, companyID, companyName, industryID, industryName string
		if err := rows.Scan(&jobID, &companyID, &companyName, &industryID, &industryName); err != nil {
			return nil, err
		}
		out[jobID] = &domain.CompanyRef{
			ID: companyID, Name: companyName,
			Industry: &domain.IndustryRef{ID: industryID, Name: industryName},
		}
	}
	return out, rows.Err()
}

func (s *Store) experiencesForJobs(ctx context.Context, ids []string) (map[string]*domain.Ref, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT j.id, e.id, e.name
		FROM jobs j JOIN job_min_experiences e ON e.id = j.min_experience_id
		WHERE j.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("experiences: %w", err)
	}
	defer rows.Close()

	out := map[string]*domain.Ref{}
	for rows.Next() {
		var jobID string
		var e domain.Ref
		if err := rows.Scan(&jobID, &e.ID, &e.Name); err != nil {
			return nil, err
		}
		out[jobID] = &e
	}
	return out, rows.Err()
}

// JobByID returns a single job with relations, or domain.ErrNotFound.
func (s *Store) JobByID(ctx context.Context, id string) (*domain.Job, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT j.id, j.url, j.company_id, j.job_position, j.employment_type,
		       j.working_location_type, j.working_location, j.min_experience_id,
		       j.min_salary, j.max_salary, j.job_desc_list, j.job_qualifications_list,
		       j.created_at, j.updated_at
		FROM jobs j WHERE j.id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("job by id: %w", err)
	}
	defer rows.Close()

	jobs, err := scanJobs(rows)
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, domain.ErrNotFound
	}
	if err := s.attachJobRelations(ctx, jobs, []string{id}); err != nil {
		return nil, err
	}
	return &jobs[0], nil
}

// Categories, experiences and industries -------------------------------------

func (s *Store) ListJobCategories(ctx context.Context) ([]domain.JobCategory, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, created_at, updated_at FROM job_categories ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.JobCategory{}
	for rows.Next() {
		var c domain.JobCategory
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&c.ID, &c.Name, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		c.CreatedAt = domain.NewMongoTime(createdAt)
		c.UpdatedAt = domain.NewMongoTime(updatedAt)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ListJobMinExperiences(ctx context.Context) ([]domain.JobMinExperience, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, created_at, updated_at FROM job_min_experiences ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.JobMinExperience{}
	for rows.Next() {
		var e domain.JobMinExperience
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&e.ID, &e.Name, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		e.CreatedAt = domain.NewMongoTime(createdAt)
		e.UpdatedAt = domain.NewMongoTime(updatedAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) ListCompanyIndustries(ctx context.Context) ([]domain.CompanyIndustry, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, created_at, updated_at FROM company_industries ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.CompanyIndustry{}
	for rows.Next() {
		var c domain.CompanyIndustry
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&c.ID, &c.Name, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		c.CreatedAt = domain.NewMongoTime(createdAt)
		c.UpdatedAt = domain.NewMongoTime(updatedAt)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListJobsByPreference is ListJobs with the tag-preference clause applied.
func (s *Store) ListJobsByPreference(ctx context.Context, f JobsFilter) ([]domain.Job, int, error) {
	return s.ListJobs(ctx, f)
}

// CategoryIDsForNames resolves category ids from LIKE patterns.
func (s *Store) CategoryIDsForNames(ctx context.Context, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM job_categories WHERE name ILIKE ANY($1)`, patterns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
