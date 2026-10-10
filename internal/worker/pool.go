// Package worker runs background jobs claimed from the Postgres queue.
//
// The first version of this service launched a bare goroutine per upload, which
// meant a deploy, crash or OOM silently discarded in-flight embeddings. Work is
// now persisted before it is acknowledged, claimed with FOR UPDATE SKIP LOCKED,
// and retried with a bounded attempt count -- so it survives restarts and scales
// to more than one replica.
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hireonai/backend-hireon-go/internal/domain"
)

// Queue is the persistence surface the worker needs.
type Queue interface {
	Claim(ctx context.Context) (*domain.QueuedJob, error)
	Complete(ctx context.Context, id string, result any) error
	Fail(ctx context.Context, id string, attempts int, reason string, maxAttempts int) error
	RequeueStale(ctx context.Context, olderThan time.Duration) (int64, error)
}

// Handler processes one kind of job.
type Handler interface {
	// Kind is the job type this handler accepts.
	Kind() string
	// Handle performs the work. Returning an error schedules a retry.
	Handle(ctx context.Context, job domain.QueuedJob) (any, error)
}

// Pool executes jobs in the background.
type Pool struct {
	queue       Queue
	handlers    map[string]Handler
	concurrency int
	maxAttempts int
	pollEvery   time.Duration

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

type Options struct {
	Concurrency int
	MaxAttempts int
	PollEvery   time.Duration
}

func New(queue Queue, handlers []Handler, opts Options) *Pool {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 2
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 3
	}
	if opts.PollEvery <= 0 {
		opts.PollEvery = 2 * time.Second
	}
	m := make(map[string]Handler, len(handlers))
	for _, h := range handlers {
		m[h.Kind()] = h
	}
	return &Pool{queue: queue, handlers: m, concurrency: opts.Concurrency,
		maxAttempts: opts.MaxAttempts, pollEvery: opts.PollEvery}
}

// Start recovers jobs stranded by a previous process and begins polling.
func (p *Pool) Start(ctx context.Context) {
	// A process that died mid-job left rows in `running`; without this they would
	// never be retried.
	if n, err := p.queue.RequeueStale(ctx, 5*time.Minute); err != nil {
		slog.Error("could not requeue stale jobs", "error", err)
	} else if n > 0 {
		slog.Info("requeued jobs stranded by a previous run", "count", n)
	}

	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	for i := 0; i < p.concurrency; i++ {
		p.wg.Add(1)
		go p.loop(runCtx, i)
	}
	slog.Info("worker pool started", "concurrency", p.concurrency)
}

// Stop waits for in-flight jobs to finish. A job already claimed is not abandoned:
// its row stays `running` and a later run requeues it.
func (p *Pool) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

func (p *Pool) loop(ctx context.Context, id int) {
	// Without this the pool's WaitGroup is never released, so Stop blocks forever
	// and shutdown hangs until the platform kills the process.
	defer p.wg.Done()

	ticker := time.NewTicker(p.pollEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Drain everything currently available before sleeping again.
			for {
				if ctx.Err() != nil {
					return
				}
				claimed, err := p.runOnce(ctx, id)
				if err != nil {
					slog.Error("worker iteration failed", "worker", id, "error", err)
					break
				}
				if !claimed {
					break
				}
			}
		}
	}
}

// runOnce claims and processes a single job. It reports whether a job was claimed.
func (p *Pool) runOnce(ctx context.Context, workerID int) (bool, error) {
	job, err := p.queue.Claim(ctx)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}

	qj := *job
	handler, ok := p.handlers[job.Kind]
	if !ok {
		// An unknown kind can never succeed; fail it immediately rather than
		// burning retries.
		slog.Error("no handler for job kind", "kind", job.Kind, "job_id", job.ID)
		return true, p.queue.Fail(ctx, job.ID, p.maxAttempts, "no handler registered for "+job.Kind, p.maxAttempts)
	}

	start := time.Now()
	result, err := handler.Handle(ctx, qj)
	if err != nil {
		retryable := job.Attempts < p.maxAttempts
		slog.Error("job failed",
			"kind", job.Kind, "job_id", job.ID, "worker", workerID,
			"attempt", job.Attempts, "retryable", retryable, "error", err)
		if ferr := p.queue.Fail(ctx, job.ID, job.Attempts, err.Error(), p.maxAttempts); ferr != nil {
			return true, ferr
		}
		return true, nil
	}

	slog.Info("job completed",
		"kind", job.Kind, "job_id", job.ID, "worker", workerID,
		"duration_ms", time.Since(start).Milliseconds())
	return true, p.queue.Complete(ctx, job.ID, result)
}
