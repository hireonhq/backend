package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hireonai/backend-hireon-go/internal/domain"
)

// Queue is a Postgres-backed work queue.
//
// The previous implementation launched a bare goroutine per upload. That loses
// work whenever the process exits: a deploy, a crash or an OOM kills in-flight
// embeddings with no record and no retry. Here the row is the source of truth and
// a worker claims it with SKIP LOCKED, so the work survives a restart and two
// replicas never process the same row.
type Queue struct {
	pool *pgxpool.Pool
}

// Enqueue inserts a pending job.
func (q *Queue) Enqueue(ctx context.Context, userID, kind string) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	_, err = q.pool.Exec(ctx,
		`INSERT INTO jobs_queue (id, user_id, kind, status) VALUES ($1,$2,$3,'pending')`, id, userID, kind)
	if err != nil {
		return "", fmt.Errorf("enqueue %s: %w", kind, err)
	}
	return id, nil
}

// Claim atomically takes the oldest runnable job, or returns nil when the queue is
// empty. FOR UPDATE SKIP LOCKED lets several workers poll concurrently without
// blocking or double-claiming.
func (q *Queue) Claim(ctx context.Context) (*domain.QueuedJob, error) {
	var j domain.QueuedJob
	err := q.pool.QueryRow(ctx, `
		WITH next AS (
		    SELECT id FROM jobs_queue
		    WHERE status = 'pending'
		       OR (status = 'running' AND updated_at < now() - interval '5 minutes')
		    ORDER BY created_at
		    FOR UPDATE SKIP LOCKED
		    LIMIT 1
		)
		UPDATE jobs_queue q
		SET status = 'running', attempts = q.attempts + 1, updated_at = now()
		FROM next WHERE q.id = next.id
		RETURNING q.id, q.user_id, q.kind, q.attempts`).
		Scan(&j.ID, &j.UserID, &j.Kind, &j.Attempts)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	return &j, nil
}

// Complete stores a result and marks the job done.
func (q *Queue) Complete(ctx context.Context, id string, result any) error {
	var payload []byte
	if result != nil {
		var err error
		payload, err = json.Marshal(result)
		if err != nil {
			return err
		}
	}
	_, err := q.pool.Exec(ctx,
		`UPDATE jobs_queue SET status='done', result=$2, error=NULL, updated_at=now() WHERE id=$1`,
		id, payload)
	return err
}

// Fail marks the job failed, or returns it to pending when attempts remain so the
// retry loop can pick it up again.
func (q *Queue) Fail(ctx context.Context, id string, attempts int, reason string, maxAttempts int) error {
	status := "failed"
	if attempts < maxAttempts {
		status = "pending"
	}
	_, err := q.pool.Exec(ctx,
		`UPDATE jobs_queue SET status=$2, error=$3, updated_at=now() WHERE id=$1`,
		id, status, reason)
	return err
}

// RequeueStale returns jobs left running by a crashed process to the queue. Called
// once at startup so a deploy cannot strand work in `running` forever.
func (q *Queue) RequeueStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := q.pool.Exec(ctx,
		`UPDATE jobs_queue SET status='pending', updated_at=now()
		 WHERE status='running' AND updated_at < now() - $1::interval`,
		olderThan.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// QueueStatus reports a job's state to its owner.
func (q *Queue) QueueStatus(ctx context.Context, userID, jobID string) (*domain.QueuedJobStatus, error) {
	var out domain.QueuedJobStatus
	var errMsg *string
	var result []byte
	err := q.pool.QueryRow(ctx, `
		SELECT id, kind, status, attempts, error, result, created_at, updated_at
		FROM jobs_queue WHERE id = $1 AND user_id = $2`, jobID, userID).
		Scan(&out.ID, &out.Kind, &out.Status, &out.Attempts, &errMsg, &result, &out.CreatedAt, &out.UpdatedAt)
	if isNoRows(err) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out.Error = errMsg
	if len(result) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(result, &payload); err == nil {
			out.Result = payload
		}
	}
	return &out, nil
}

// OldestPendingAge reports how long the oldest pending job has waited, for the
// readiness/health endpoint.
func (q *Queue) OldestPendingAge(ctx context.Context) (time.Duration, error) {
	var seconds *float64
	err := q.pool.QueryRow(ctx, `
		SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) FROM jobs_queue WHERE status='pending'`).
		Scan(&seconds)
	if err != nil || seconds == nil {
		return 0, err
	}
	return time.Duration(*seconds * float64(time.Second)), nil
}
