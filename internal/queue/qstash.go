package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// QStashConfig configures the QStash backend.
type QStashConfig struct {
	// Token is the QStash publish token (https://console.upstash.com/qstash).
	Token string

	// WebhookBaseURL is the public URL the engine is reachable at. QStash
	// will POST jobs to WebhookBaseURL + "/internal/jobs/:kind".
	WebhookBaseURL string

	// HTTPClient is optional; if nil, http.DefaultClient is used.
	HTTPClient *http.Client
}

// QStash is a serverless-friendly Queue backed by Upstash QStash.
//
// It publishes jobs by POSTing to https://qstash.upstash.io/v2/publish/<url>.
// When QStash fires, it POSTs the job back to the engine's webhook endpoint,
// where handlers dispatch by JobKind.
type QStash struct {
	cfg      QStashConfig
	client   *http.Client
	mu       sync.RWMutex
	handlers map[JobKind]Handler
}

// NewQStash constructs a new QStash-backed Queue.
func NewQStash(cfg QStashConfig) (*QStash, error) {
	if cfg.Token == "" {
		return nil, errors.New("qstash: token is required")
	}
	if cfg.WebhookBaseURL == "" {
		return nil, errors.New("qstash: webhook_base_url is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &QStash{
		cfg:      cfg,
		client:   client,
		handlers: map[JobKind]Handler{},
	}, nil
}

func (q *QStash) Register(kind JobKind, h Handler) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handlers[kind] = h
}

// Enqueue publishes a job to QStash for eventual delivery to the engine's
// webhook endpoint.
func (q *QStash) Enqueue(ctx context.Context, j Job) error {
	body, err := json.Marshal(j)
	if err != nil {
		return err
	}

	dest := fmt.Sprintf("%s/internal/jobs/%s", q.cfg.WebhookBaseURL, j.Kind)
	publishURL := "https://qstash.upstash.io/v2/publish/" + dest

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, publishURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+q.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	if !j.RunAt.IsZero() {
		delay := time.Until(j.RunAt)
		if delay > 0 {
			req.Header.Set("Upstash-Delay", fmt.Sprintf("%ds", int(delay.Seconds())))
		}
	}
	if j.DedupeKey != "" {
		req.Header.Set("Upstash-Deduplication-Id", j.DedupeKey)
	}
	if j.MaxRetries > 0 {
		req.Header.Set("Upstash-Retries", fmt.Sprintf("%d", j.MaxRetries))
	}

	resp, err := q.client.Do(req)
	if err != nil {
		return fmt.Errorf("qstash publish: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("qstash publish: status=%d body=%s", resp.StatusCode, string(b))
	}
	return nil
}

// Start is a no-op for QStash. Delivery is push-based: QStash calls the
// engine's webhook endpoint, which invokes Dispatch directly.
func (q *QStash) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (q *QStash) Close() error { return nil }

// Dispatch is invoked by the engine's webhook handler for a QStash callback.
// It looks up the handler for the given kind and executes it synchronously.
func (q *QStash) Dispatch(ctx context.Context, kind JobKind, payload []byte) error {
	q.mu.RLock()
	h, ok := q.handlers[kind]
	q.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownKind, kind)
	}
	return h(ctx, Job{Kind: kind, Payload: payload})
}
