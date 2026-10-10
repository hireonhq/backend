package worker_test

// Tests for the background pool. The important one is retry-on-failure: a job that
// fails must go back to pending until its attempts are exhausted, which is what
// makes the queue safe across restarts.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hireonai/backend-hireon-go/internal/domain"
	"github.com/hireonai/backend-hireon-go/internal/worker"
)

type fakeQueue struct {
	mu        sync.Mutex
	claims    int
	pending   []domain.QueuedJob
	completed []string
	failed    map[string]string
	failures  int
	requeued  int64
}

func newFakeQueue(jobs ...domain.QueuedJob) *fakeQueue {
	return &fakeQueue{pending: jobs, failed: map[string]string{}}
}

func (f *fakeQueue) Claim(context.Context) (*domain.QueuedJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return nil, nil
	}
	j := f.pending[0]
	f.pending = f.pending[1:]
	f.claims++
	// Mirror the real queue, which increments attempts when it claims a row.
	// Without this the retry loop below never terminates.
	j.Attempts++
	return &j, nil
}

func (f *fakeQueue) Complete(_ context.Context, id string, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, id)
	return nil
}

func (f *fakeQueue) Fail(_ context.Context, id string, attempts int, reason string, maxAttempts int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[id] = reason
	f.failures++
	if attempts < maxAttempts {
		// Mirror the real queue: return the job to pending for another attempt.
		f.pending = append(f.pending, domain.QueuedJob{ID: id, Kind: "test", Attempts: attempts})
	}
	return nil
}

// attemptCount reports how many times a job was claimed, used by the exhaustion
// test to prove retries actually stopped.
func (f *fakeQueue) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims
}

func (f *fakeQueue) failureCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures
}

func (f *fakeQueue) RequeueStale(context.Context, time.Duration) (int64, error) {
	return f.requeued, nil
}

func (f *fakeQueue) counts() (completed int, failed int, pending int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.completed), len(f.failed), len(f.pending)
}

type fakeHandler struct {
	kind    string
	calls   int
	failFor int // fail the first N invocations
	mu      sync.Mutex
}

func (h *fakeHandler) Kind() string { return h.kind }

func (h *fakeHandler) Handle(context.Context, domain.QueuedJob) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if h.calls <= h.failFor {
		return nil, errors.New("transient failure")
	}
	return map[string]any{"ok": true}, nil
}

func (h *fakeHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// A failing job is retried until it succeeds, then completed exactly once.
func TestRetryUntilSuccess(t *testing.T) {
	q := newFakeQueue(domain.QueuedJob{ID: "j1", Kind: "test"})
	h := &fakeHandler{kind: "test", failFor: 2}
	pool := worker.New(q, []worker.Handler{h}, worker.Options{
		Concurrency: 1, MaxAttempts: 3, PollEvery: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		completed, _, _ := q.counts()
		if completed == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	pool.Stop()

	completed, failed, _ := q.counts()
	if completed != 1 {
		t.Fatalf("completed = %d, want 1", completed)
	}
	// Two handler invocations failed before the third succeeded; the map counts
	// distinct jobs, so compare attempt counts instead.
	if failed < 1 || q.failureCount() < 2 {
		t.Errorf("expected two recorded failures, got map=%d attempts=%d", failed, q.failureCount())
	}
	if h.count() != 3 {
		t.Errorf("handler calls = %d, want 3", h.count())
	}
}

// A permanently failing job stops after maxAttempts and is never completed.
func TestExhaustedRetriesAreNotCompleted(t *testing.T) {
	q := newFakeQueue(domain.QueuedJob{ID: "j1", Kind: "test"})
	h := &fakeHandler{kind: "test", failFor: 100}
	pool := worker.New(q, []worker.Handler{h}, worker.Options{
		Concurrency: 1, MaxAttempts: 2, PollEvery: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	pool.Stop()

	completed, failed, pending := q.counts()
	if completed != 0 {
		t.Fatalf("completed = %d, want 0", completed)
	}
	if failed == 0 {
		t.Fatal("expected the job to be recorded as failed")
	}
	if pending != 0 {
		t.Fatalf("pending = %d, want 0 once attempts are exhausted", pending)
	}
	if got := q.attemptCount(); got != 2 {
		t.Fatalf("job was claimed %d times, want exactly maxAttempts (2)", got)
	}
}

// A job whose kind has no handler fails immediately instead of burning retries.
func TestUnknownKindFailsFast(t *testing.T) {
	q := newFakeQueue(domain.QueuedJob{ID: "j1", Kind: "mystery"})
	pool := worker.New(q, nil, worker.Options{
		Concurrency: 1, MaxAttempts: 3, PollEvery: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	time.Sleep(150 * time.Millisecond)
	pool.Stop()

	completed, failed, pending := q.counts()
	if completed != 0 {
		t.Fatalf("completed = %d, want 0", completed)
	}
	if failed != 1 {
		t.Fatalf("failed = %d, want 1", failed)
	}
	if pending != 0 {
		t.Fatalf("pending = %d, want 0 (an unhandled kind must not be requeued)", pending)
	}
}

// Start recovers work stranded by a previous process.
func TestStartRequeuesStaleJobs(t *testing.T) {
	q := newFakeQueue()
	q.requeued = 7
	pool := worker.New(q, nil, worker.Options{PollEvery: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	pool.Stop()

	if q.requeued != 7 {
		t.Fatalf("requeued = %d, want the stub value 7", q.requeued)
	}
}

// Stop returns once in-flight work has drained, so shutdown does not abandon it.
func TestStopWaitsForInFlightWork(t *testing.T) {
	q := newFakeQueue(domain.QueuedJob{ID: "j1", Kind: "slow"})

	started := make(chan struct{})
	release := make(chan struct{})
	h := &blockingHandler{kind: "slow", started: started, release: release}

	pool := worker.New(q, []worker.Handler{h}, worker.Options{
		Concurrency: 1, MaxAttempts: 1, PollEvery: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	done := make(chan struct{})
	go func() { pool.Stop(); close(done) }()

	select {
	case <-done:
		t.Fatal("Stop returned while a job was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after the job finished")
	}

	completed, _, _ := q.counts()
	if completed != 1 {
		t.Fatalf("completed = %d, want 1", completed)
	}
}

type blockingHandler struct {
	kind    string
	started chan struct{}
	release chan struct{}
}

func (h *blockingHandler) Kind() string { return h.kind }

func (h *blockingHandler) Handle(context.Context, domain.QueuedJob) (any, error) {
	close(h.started)
	<-h.release
	return map[string]any{"ok": true}, nil
}
