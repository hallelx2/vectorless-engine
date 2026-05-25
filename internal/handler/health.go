package handler

import (
	"net/http"
)

// HealthHandler serves liveness and version endpoints.
type HealthHandler struct {
	version string
}

// NewHealthHandler creates a HealthHandler with the given build version.
func NewHealthHandler(version string) *HealthHandler {
	return &HealthHandler{version: version}
}

// HandleHealth responds with {"status": "ok"}. Used as a liveness
// probe by Kubernetes and load-balancer health checks.
func (h *HealthHandler) HandleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HandleVersion responds with the server build version.
func (h *HealthHandler) HandleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": h.version})
}
