package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hallelx2/vectorless-engine/pkg/queue"
)

// WebhookHandler handles inbound queue webhooks (e.g. QStash).
type WebhookHandler struct {
	logger *slog.Logger
	queue  queue.Queue
}

// NewWebhookHandler creates a WebhookHandler.
func NewWebhookHandler(logger *slog.Logger, q queue.Queue) *WebhookHandler {
	return &WebhookHandler{logger: logger, queue: q}
}

// HandleQueueWebhook is the endpoint QStash POSTs to. It verifies the
// Upstash-Signature header, decodes the payload, and dispatches to the
// registered handler for {kind}.
//
// Only active when the configured queue is *queue.QStash; other
// drivers return 404.
func (h *WebhookHandler) HandleQueueWebhook(w http.ResponseWriter, r *http.Request) {
	qq, ok := h.queue.(*queue.QStash)
	if !ok {
		writeErr(w, http.StatusNotFound, "webhook not enabled: queue driver is not qstash")
		return
	}

	kind := queue.JobKind(chi.URLParam(r, "kind"))
	if kind == "" {
		writeErr(w, http.StatusBadRequest, "missing kind")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	_ = r.Body.Close()

	// Signature verification. Without a signing key we refuse to
	// process the webhook — an unauthenticated endpoint that
	// dispatches jobs is an open door.
	v := qq.Verifier()
	if v == nil {
		writeErr(w, http.StatusUnauthorized, "qstash signing key not configured")
		return
	}

	expectedURL := strings.TrimRight(qq.WebhookBaseURL(), "/") + r.URL.Path
	sig := r.Header.Get("Upstash-Signature")
	if err := v.Verify(sig, body, expectedURL); err != nil {
		if h.logger != nil {
			h.logger.Warn("qstash verify failed", "err", err, "kind", kind)
		}
		writeErr(w, http.StatusUnauthorized, "invalid qstash signature")
		return
	}

	// Accept both wrapped {kind, payload} and bare payload bodies.
	payload := body
	var maybe struct {
		Kind    queue.JobKind   `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(body, &maybe); err == nil && maybe.Kind != "" && len(maybe.Payload) > 0 {
		if maybe.Kind != kind {
			writeErr(w, http.StatusBadRequest, "kind in body does not match URL")
			return
		}
		payload = maybe.Payload
	}

	if err := qq.Dispatch(r.Context(), kind, payload); err != nil {
		if errors.Is(err, queue.ErrUnknownKind) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		if h.logger != nil {
			h.logger.Error("qstash dispatch failed", "err", err, "kind", kind)
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
