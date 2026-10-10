package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hireonai/backend-hireon-go/internal/domain"
)

// UpsertUserFromSupabase resolves a Supabase identity to a local user, creating
// the user and profile on first sight.
//
// The Node version did find-then-create with an application-level uniqueness
// retry loop for the username, which races under concurrent first requests:
// two requests could both see "free" and one insert would fail. This uses
// ON CONFLICT so the database enforces it, and derives a unique username with a
// bounded suffix loop inside the same statement.
func (s *Store) UpsertUserFromSupabase(ctx context.Context, sub, email, fullname, picture, provider string) (*domain.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var user domain.User
	err = tx.QueryRow(ctx,
		`SELECT id, username, email, coalesce(supabase_id,''), role,
		        coalesce(photo_url,''), coalesce(oauth_provider,''), verified_at
		 FROM users WHERE supabase_id = $1`, sub).
		Scan(&user.ID, &user.Username, &user.Email, &user.SupabaseID, &user.Role,
			&user.PhotoURL, &user.OAuthProvider, &user.VerifiedAt)
	if err == nil {
		return &user, tx.Commit(ctx)
	}
	if !isNoRows(err) {
		return nil, fmt.Errorf("find user by supabase id: %w", err)
	}

	// Link an existing email account (e.g. created before Supabase migration).
	err = tx.QueryRow(ctx,
		`UPDATE users SET supabase_id = $2, updated_at = now(),
		        photo_url = COALESCE(photo_url, NULLIF($3,''))
		 WHERE email = $1 AND supabase_id IS NULL
		 RETURNING id, username, email, coalesce(supabase_id,''), role,
		           coalesce(photo_url,''), coalesce(oauth_provider,''), verified_at`,
		email, sub, picture).
		Scan(&user.ID, &user.Username, &user.Email, &user.SupabaseID, &user.Role,
			&user.PhotoURL, &user.OAuthProvider, &user.VerifiedAt)
	if err == nil {
		return &user, tx.Commit(ctx)
	}
	if !isNoRows(err) {
		return nil, fmt.Errorf("link user by email: %w", err)
	}

	userID, err := newID()
	if err != nil {
		return nil, err
	}
	base := usernameFromEmail(email)
	username := base
	for attempt := 0; attempt < 50; attempt++ {
		if attempt > 0 {
			username = fmt.Sprintf("%s%d", base, attempt)
		}
		err = tx.QueryRow(ctx,
			`INSERT INTO users (id, username, email, supabase_id, role, oauth_provider, photo_url, verified_at)
			 VALUES ($1,$2,$3,$4,'seeker',$5,NULLIF($6,''),now())
			 ON CONFLICT (username) DO NOTHING
			 RETURNING id, username, email, coalesce(supabase_id,''), role,
			           coalesce(photo_url,''), coalesce(oauth_provider,''), verified_at`,
			userID, username, email, sub, provider, picture).
			Scan(&user.ID, &user.Username, &user.Email, &user.SupabaseID, &user.Role,
				&user.PhotoURL, &user.OAuthProvider, &user.VerifiedAt)
		if err == nil {
			break
		}
		if !isNoRows(err) {
			return nil, fmt.Errorf("create user: %w", err)
		}
	}
	if user.ID == "" {
		return nil, fmt.Errorf("could not allocate a unique username for %q", base)
	}

	profileID, err := newID()
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO profiles (id, user_id, fullname, photo_url) VALUES ($1,$2,$3,NULLIF($4,''))
		 ON CONFLICT (user_id) DO NOTHING`,
		profileID, user.ID, fullname, picture); err != nil {
		return nil, fmt.Errorf("create profile: %w", err)
	}
	return &user, tx.Commit(ctx)
}

func (s *Store) UserBySupabaseID(ctx context.Context, sub string) (*domain.User, error) {
	var u domain.User
	err := s.pool.QueryRow(ctx,
		`SELECT id, username, email, coalesce(supabase_id,''), role,
		        coalesce(photo_url,''), coalesce(oauth_provider,''), verified_at
		 FROM users WHERE supabase_id = $1`, sub).
		Scan(&u.ID, &u.Username, &u.Email, &u.SupabaseID, &u.Role, &u.PhotoURL, &u.OAuthProvider, &u.VerifiedAt)
	if isNoRows(err) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ProfileByUserID loads a profile with its user and bookmarked jobs.
func (s *Store) ProfileByUserID(ctx context.Context, userID string) (*domain.Profile, error) {
	var p domain.Profile
	var u domain.UserRef
	var createdAt, updatedAt time.Time
	var verifiedAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT p.id, p.fullname, p.phone, p.domicile, p.last_education, p.photo_url,
		       p.portfolio_url, p.cv_url, p.cv_status, p.tag_preferences,
		       p.created_at, p.updated_at,
		       u.id, u.username, u.email, u.verified_at
		FROM profiles p JOIN users u ON u.id = p.user_id
		WHERE p.user_id = $1`, userID).
		Scan(&p.ID, &p.Fullname, &p.Phone, &p.Domicile, &p.LastEducation, &p.PhotoURL,
			&p.PortfolioURL, &p.CVURL, &p.CVStatus, &p.TagPreferences,
			&createdAt, &updatedAt,
			&u.ID, &u.Username, &u.Email, &verifiedAt)
	if err == nil {
		p.CreatedAt = domain.NewMongoTime(createdAt)
		p.UpdatedAt = domain.NewMongoTime(updatedAt)
		if verifiedAt != nil {
			mt := domain.NewMongoTime(*verifiedAt)
			u.VerifiedAt = &mt
		}
	}
	if isNoRows(err) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("profile by user: %w", err)
	}
	p.User = &u

	bookmarks, err := s.bookmarkedJobs(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	p.BookmarkJobs = bookmarks
	return &p, nil
}

// bookmarkedJobs mirrors the Node populate: only url, jobPosition, employmentType
// and the company name, ordered by the time they were bookmarked.
func (s *Store) bookmarkedJobs(ctx context.Context, profileID string) ([]domain.Job, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT j.id, j.url, j.job_position, j.employment_type, c.id, c.name
		FROM bookmark_jobs b
		JOIN jobs j ON j.id = b.job_id
		JOIN companies c ON c.id = j.company_id
		WHERE b.profile_id = $1
		ORDER BY b.created_at DESC`, profileID)
	if err != nil {
		return nil, fmt.Errorf("bookmarked jobs: %w", err)
	}
	defer rows.Close()

	out := []domain.Job{}
	for rows.Next() {
		var j domain.Job
		var company domain.CompanyRef
		if err := rows.Scan(&j.ID, &j.URL, &j.JobPosition, &j.EmploymentType, &company.ID, &company.Name); err != nil {
			return nil, err
		}
		j.Company = &company
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) UpdateProfileFields(ctx context.Context, userID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	set := ""
	args := []any{userID}
	i := 2
	for _, col := range []string{"fullname", "phone", "domicile", "last_education", "portfolio_url"} {
		v, ok := fields[col]
		if !ok {
			continue
		}
		if set != "" {
			set += ", "
		}
		set += fmt.Sprintf("%s = $%d", col, i)
		args = append(args, v)
		i++
	}
	if set == "" {
		return nil
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE profiles SET `+set+`, updated_at = now() WHERE user_id = $1`, args...)
	if err != nil {
		return fmt.Errorf("update profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) UpdateTagPreferences(ctx context.Context, userID string, tags []string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE profiles SET tag_preferences = $2, updated_at = now() WHERE user_id = $1`, userID, tags)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// AddBookmark is idempotent; bookmarking the same job twice is not an error.
func (s *Store) AddBookmark(ctx context.Context, userID, jobID string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`, jobID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return domain.ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO bookmark_jobs (profile_id, job_id)
		SELECT p.id, $2 FROM profiles p WHERE p.user_id = $1
		ON CONFLICT DO NOTHING`, userID, jobID)
	if err != nil {
		return fmt.Errorf("add bookmark: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the profile is missing or the bookmark already existed.
		var profileExists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM profiles WHERE user_id=$1)`, userID).Scan(&profileExists); err != nil {
			return err
		}
		if !profileExists {
			return domain.ErrNotFound
		}
	}
	return nil
}

func (s *Store) RemoveBookmark(ctx context.Context, userID, jobID string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM bookmark_jobs b USING profiles p
		WHERE b.profile_id = p.id AND p.user_id = $1 AND b.job_id = $2`, userID, jobID)
	if err != nil {
		return fmt.Errorf("remove bookmark: %w", err)
	}
	return nil
}

// SetCV updates the CV URL and embedding status in one statement.
func (s *Store) SetCV(ctx context.Context, userID string, cvURL *string, status string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE profiles SET cv_url = $2, cv_status = $3, updated_at = now() WHERE user_id = $1`,
		userID, cvURL, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) SetPhoto(ctx context.Context, userID string, photoURL string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE profiles SET photo_url = $2, updated_at = now() WHERE user_id = $1`, userID, photoURL)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) ProfileCV(ctx context.Context, userID string) (string, string, error) {
	var url *string
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT cv_url, cv_status FROM profiles WHERE user_id = $1`, userID).Scan(&url, &status)
	if isNoRows(err) {
		return "", "", domain.ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	if url == nil {
		return "", status, nil
	}
	return *url, status, nil
}

// AnalysisResultFor returns the stored analysis for a user/job pair, if any.
func (s *Store) AnalysisResultFor(ctx context.Context, userID, jobID string) (*domain.AnalysisResult, error) {
	var a domain.AnalysisResult
	var createdAt, updatedAt time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, job_id, cv_relevance_score, explanation,
		       skil_identification_dict, suggestions, improvements, created_at, updated_at
		FROM analysis_results WHERE user_id = $1 AND job_id = $2`, userID, jobID).
		Scan(&a.ID, &a.UserID, &a.JobID, &a.CVRelevanceScore, &a.Explanation,
			&a.SkilIdentificationDict, &a.Suggestions, &a.Improvements, &createdAt, &updatedAt)
	if err == nil {
		a.CreatedAt = domain.NewMongoTime(createdAt)
		a.UpdatedAt = domain.NewMongoTime(updatedAt)
	}
	if isNoRows(err) {
		return nil, nil // absence is normal, not an error
	}
	if err != nil {
		return nil, fmt.Errorf("analysis result: %w", err)
	}
	return &a, nil
}

func (s *Store) UpsertAnalysisResult(ctx context.Context, a *domain.AnalysisResult) (*domain.AnalysisResult, error) {
	if a.ID == "" {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		a.ID = id
	}
	var out domain.AnalysisResult
	var createdAt, updatedAt time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO analysis_results (id, user_id, job_id, cv_relevance_score, explanation,
		   skil_identification_dict, suggestions, improvements)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (user_id, job_id) DO UPDATE SET
		   cv_relevance_score = EXCLUDED.cv_relevance_score,
		   explanation = EXCLUDED.explanation,
		   skil_identification_dict = EXCLUDED.skil_identification_dict,
		   suggestions = EXCLUDED.suggestions,
		   improvements = EXCLUDED.improvements,
		   updated_at = now()
		RETURNING id, user_id, job_id, cv_relevance_score, explanation,
		          skil_identification_dict, suggestions, improvements, created_at, updated_at`,
		a.ID, a.UserID, a.JobID, a.CVRelevanceScore, a.Explanation,
		a.SkilIdentificationDict, a.Suggestions, a.Improvements).
		Scan(&out.ID, &out.UserID, &out.JobID, &out.CVRelevanceScore, &out.Explanation,
			&out.SkilIdentificationDict, &out.Suggestions, &out.Improvements, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert analysis result: %w", err)
	}
	out.CreatedAt = domain.NewMongoTime(createdAt)
	out.UpdatedAt = domain.NewMongoTime(updatedAt)
	return &out, nil
}

// EnqueueJob records a background unit of work and returns its id.
func (s *Store) EnqueueJob(ctx context.Context, userID, kind string) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO jobs_queue (id, user_id, kind, status) VALUES ($1,$2,$3,'pending')`,
		id, userID, kind); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) JobStatus(ctx context.Context, userID, queueID string) (string, *string, []byte, error) {
	var status string
	var errMsg *string
	var result []byte
	err := s.pool.QueryRow(ctx,
		`SELECT status, error, result FROM jobs_queue WHERE id = $1 AND user_id = $2`,
		queueID, userID).Scan(&status, &errMsg, &result)
	if isNoRows(err) {
		return "", nil, nil, domain.ErrNotFound
	}
	if err != nil {
		return "", nil, nil, err
	}
	return status, errMsg, result, nil
}

var _ = pgx.ErrNoRows

// BookmarkedJobs returns the caller's bookmarked jobs in bookmark order,
// mirroring the Node populate projection (url, jobPosition, employmentType,
// company name only).
func (s *Store) BookmarkedJobs(ctx context.Context, userID string) ([]domain.Job, error) {
	var profileID string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM profiles WHERE user_id = $1`, userID).Scan(&profileID); err != nil {
		if isNoRows(err) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return s.bookmarkedJobs(ctx, profileID)
}

func (s *Store) SetCVStatus(ctx context.Context, userID, status string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE profiles SET cv_status = $2, updated_at = now() WHERE user_id = $1`, userID, status)
	return err
}

func (s *Store) CompleteJob(ctx context.Context, jobID string) {
	_, _ = s.pool.Exec(ctx,
		`UPDATE jobs_queue SET status='done', attempts=attempts+1, updated_at=now() WHERE id=$1`, jobID)
}

func (s *Store) FailJob(ctx context.Context, jobID, reason string) {
	_, _ = s.pool.Exec(ctx,
		`UPDATE jobs_queue SET status='failed', attempts=attempts+1, error=$2, updated_at=now() WHERE id=$1`,
		jobID, reason)
}
