package postgres_test

// Integration tests for the durable queue. The restart test is the reason the
// queue exists: the previous implementation ran embeddings in a bare goroutine,
// so a deploy or crash discarded the work with no record and no retry.

import (
	"context"
	"testing"
	"time"

	"github.com/hireonai/backend-hireon-go/internal/domain"
)

func TestQueueEnqueueAndClaim(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	q := store.Queue()
	userID := seededUserID(t)

	id, err := q.Enqueue(ctx, userID, "cv_embedding")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	t.Cleanup(func() { cleanupJob(t, id) })

	job, err := q.Claim(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job == nil {
		t.Fatal("job was not claimable")
	}
	if job.ID != id || job.Kind != "cv_embedding" {
		t.Fatalf("claimed %+v, want id=%s kind=cv_embedding", job, id)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 after the first claim", job.Attempts)
	}
}

// A job that fails with attempts remaining returns to pending so the next worker
// picks it up again.
func TestQueueRetryableFailure(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	q := store.Queue()
	userID := seededUserID(t)

	id, err := q.Enqueue(ctx, userID, "cv_embedding")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	t.Cleanup(func() { cleanupJob(t, id) })

	job, err := q.Claim(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: %v (job=%v)", err, job)
	}
	if err := q.Fail(ctx, job.ID, job.Attempts, "transient", 3); err != nil {
		t.Fatalf("fail: %v", err)
	}

	again, err := q.Claim(ctx)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if again == nil {
		t.Fatal("a retryable job was not returned to the queue")
	}
	if again.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", again.Attempts)
	}
}

// Once attempts are exhausted the job stays failed and is never claimed again.
func TestQueueExhaustsRetries(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	q := store.Queue()
	userID := seededUserID(t)

	id, err := q.Enqueue(ctx, userID, "cv_embedding")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	t.Cleanup(func() { cleanupJob(t, id) })

	job, _ := q.Claim(ctx)
	if job == nil {
		t.Fatal("expected a claimable job")
	}
	if err := q.Fail(ctx, job.ID, job.Attempts, "permanent", 1); err != nil {
		t.Fatalf("fail: %v", err)
	}

	next, err := q.Claim(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if next != nil && next.ID == id {
		t.Fatal("an exhausted job was claimed again")
	}

	status, err := q.QueueStatus(ctx, userID, id)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Status != "failed" {
		t.Errorf("status = %q, want failed", status.Status)
	}
}

// A job marked running by a process that then died is recovered.
func TestQueueRecoversStaleRunning(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	q := store.Queue()
	userID := seededUserID(t)

	id, err := q.Enqueue(ctx, userID, "cv_embedding")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	t.Cleanup(func() { cleanupJob(t, id) })

	if _, err := q.Claim(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}
	simulateStale(t, id, 10*time.Minute)

	n, err := q.RequeueStale(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if n < 1 {
		t.Fatalf("requeued = %d, want at least 1", n)
	}
	status, err := q.QueueStatus(ctx, userID, id)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Status != "pending" {
		t.Errorf("status = %q, want pending after recovery", status.Status)
	}
}

// Completing a job records its result.
func TestQueueCompleteStoresResult(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	q := store.Queue()
	userID := seededUserID(t)

	id, err := q.Enqueue(ctx, userID, "cv_embedding")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	t.Cleanup(func() { cleanupJob(t, id) })

	job, _ := q.Claim(ctx)
	if job == nil {
		t.Fatal("expected a claimable job")
	}
	if err := q.Complete(ctx, id, map[string]any{"cvUrl": "https://cdn/cv.pdf"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	status, err := q.QueueStatus(ctx, userID, id)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Status != "done" {
		t.Errorf("status = %q, want done", status.Status)
	}
	if status.Result["cvUrl"] != "https://cdn/cv.pdf" {
		t.Errorf("result = %v, want the stored payload", status.Result)
	}
}

// Status for someone else's job is not disclosed.
func TestQueueStatusScopedToOwner(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	q := store.Queue()
	userID := seededUserID(t)

	id, err := q.Enqueue(ctx, userID, "cv_embedding")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	t.Cleanup(func() { cleanupJob(t, id) })

	if _, err := q.QueueStatus(ctx, "000000000000000000000000", id); err == nil {
		t.Fatal("another user's job status must not be readable")
	}
}

var _ = domain.ErrNotFound
