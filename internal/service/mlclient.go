package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/hireonai/backend-hireon-go/internal/config"
	"github.com/hireonai/backend-hireon-go/internal/domain"
	"github.com/hireonai/backend-hireon-go/internal/httpx"
)

// MLRecommender calls the ML service's recommendation endpoint.
//
// Every call has an explicit timeout. The Node backend set no axios timeout
// anywhere, so a stalled ML service would hold the request open until the
// platform killed it, piling up connections.
type MLRecommender struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewMLRecommender(cfg config.Config) *MLRecommender {
	return &MLRecommender{
		baseURL: cfg.MLServiceURL,
		apiKey:  cfg.MLServiceKey,
		client: &http.Client{
			Timeout: cfg.MLLatencyBudget,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

type mlRecommendationsResponse struct {
	Recommendations []struct {
		JobID           string  `json:"job_id"`
		SimilarityScore float64 `json:"similarity_score"`
	} `json:"recommendations"`
}

// Recommend fetches ranked job ids for a user.
//
// Failure modes are mapped to distinct sentinels so the handler can answer 502
// or 504 instead of forwarding an upstream status and body verbatim.
func (m *MLRecommender) Recommend(ctx context.Context, userID string) ([]Recommendation, error) {
	endpoint := fmt.Sprintf("%s/recommendation-engine/recommendations?user_id=%s",
		m.baseURL, url.QueryEscape(userID))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, domain.ErrUpstream
	}
	req.Header.Set("X-API-Key", m.apiKey)
	// Correlate this call with the originating API request in both log streams.
	if rid := httpx.RequestID(ctx); rid != "" {
		req.Header.Set("X-Request-Id", rid)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, domain.ErrUpstreamSlow
		}
		return nil, domain.ErrUpstream
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, domain.ErrUpstream
	}

	var payload mlRecommendationsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, domain.ErrUpstream
	}

	out := make([]Recommendation, 0, len(payload.Recommendations))
	for _, r := range payload.Recommendations {
		out = append(out, Recommendation{JobID: r.JobID, Score: r.SimilarityScore})
	}
	return out, nil
}

func isTimeout(err error) bool {
	type timeouter interface{ Timeout() bool }
	var t timeouter
	return errors.As(err, &t) && t.Timeout()
}

// EmbedCV asks the ML service to build and store a CV embedding.
//
// This is the queued path: it runs in a background goroutine with its own
// timeout, so a slow ML service delays only the embedding, never the upload
// response. The Node backend awaited this inline with no timeout at all.
func (m *MLRecommender) EmbedCV(ctx context.Context, userID, cvURL string) error {
	payload, err := json.Marshal(map[string]string{
		"cv_storage_url": cvURL,
		"user_id":        userID,
	})
	if err != nil {
		return domain.ErrUpstream
	}

	endpoint := m.baseURL + "/recommendation-engine/cv_embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return domain.ErrUpstream
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", m.apiKey)
	if rid := httpx.RequestID(ctx); rid != "" {
		req.Header.Set("X-Request-Id", rid)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return domain.ErrUpstreamSlow
		}
		return domain.ErrUpstream
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return domain.ErrUpstream
	}
	return nil
}
