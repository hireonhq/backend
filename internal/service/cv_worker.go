package service

import (
	"context"
	"log/slog"

	"github.com/hireonai/backend-hireon-go/internal/domain"
	"github.com/hireonai/backend-hireon-go/internal/worker"
)

// CVEmbeddingHandler builds and stores a CV embedding for the queued job.
//
// This is the replacement for the Node flow, which called the ML service inline
// during the upload request with no timeout: a slow or unavailable ML service
// either hung the request or failed it after the file had already been stored.
type CVEmbeddingHandler struct {
	store    CVEmbeddingStore
	embedder CVEmbedder
}

// CVEmbeddingStore is the persistence the handler needs.
type CVEmbeddingStore interface {
	ProfileCV(ctx context.Context, userID string) (string, string, error)
	SetCVStatus(ctx context.Context, userID, status string) error
}

func NewCVEmbeddingHandler(store CVEmbeddingStore, embedder CVEmbedder) *CVEmbeddingHandler {
	return &CVEmbeddingHandler{store: store, embedder: embedder}
}

func (h *CVEmbeddingHandler) Kind() string { return "cv_embedding" }

func (h *CVEmbeddingHandler) Handle(ctx context.Context, job domain.QueuedJob) (any, error) {
	cvURL, _, err := h.store.ProfileCV(ctx, job.UserID)
	if err != nil {
		// The profile is gone; retrying cannot help.
		_ = h.store.SetCVStatus(ctx, job.UserID, "failed")
		return nil, nil
	}
	if cvURL == "" {
		// A newer upload superseded this job; the CV is intentionally absent.
		slog.Info("cv embedding skipped, no cv on profile", "user_id", job.UserID, "job_id", job.ID)
		return map[string]any{"skipped": "no cv"}, nil
	}
	if h.embedder == nil {
		_ = h.store.SetCVStatus(ctx, job.UserID, "failed")
		return nil, nil
	}

	if err := h.embedder.EmbedCV(ctx, job.UserID, cvURL); err != nil {
		_ = h.store.SetCVStatus(ctx, job.UserID, "failed")
		return nil, err // retried by the pool until maxAttempts
	}

	_ = h.store.SetCVStatus(ctx, job.UserID, "ready")
	return map[string]any{"cvUrl": cvURL}, nil
}

var _ worker.Handler = (*CVEmbeddingHandler)(nil)
