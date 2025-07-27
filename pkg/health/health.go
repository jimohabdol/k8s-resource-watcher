package health

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// Handler manages health check endpoints
type Handler struct {
	ready     atomic.Bool
	startTime time.Time
}

// HealthStatus represents the health status of the application
type HealthStatus struct {
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	Uptime    string    `json:"uptime"`
}

// NewHandler creates a new health check handler
func NewHandler() *Handler {
	return &Handler{
		startTime: time.Now(),
	}
}

// SetReady marks the application as ready
func (h *Handler) SetReady(ready bool) {
	h.ready.Store(ready)
}

// LivenessHandler handles liveness probe requests
func (h *Handler) LivenessHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	status := HealthStatus{
		Status:    "alive",
		Timestamp: time.Now(),
		Uptime:    time.Since(h.startTime).String(),
	}

	json.NewEncoder(w).Encode(status)
}

// ReadinessHandler handles readiness probe requests
func (h *Handler) ReadinessHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if h.ready.Load() {
		w.WriteHeader(http.StatusOK)
		status := HealthStatus{
			Status:    "ready",
			Timestamp: time.Now(),
			Uptime:    time.Since(h.startTime).String(),
		}
		json.NewEncoder(w).Encode(status)
		return
	}

	w.WriteHeader(http.StatusServiceUnavailable)
	status := HealthStatus{
		Status:    "not ready",
		Timestamp: time.Now(),
		Uptime:    time.Since(h.startTime).String(),
	}
	json.NewEncoder(w).Encode(status)
}
