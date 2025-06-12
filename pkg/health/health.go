package health

import (
	"net/http"
	"sync/atomic"
)

// Handler manages health check endpoints
type Handler struct {
	ready atomic.Bool
}

// NewHandler creates a new health check handler
func NewHandler() *Handler {
	return &Handler{}
}

// SetReady marks the application as ready
func (h *Handler) SetReady(ready bool) {
	h.ready.Store(ready)
}

// LivenessHandler handles liveness probe requests
func (h *Handler) LivenessHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("alive"))
}

// ReadinessHandler handles readiness probe requests
func (h *Handler) ReadinessHandler(w http.ResponseWriter, r *http.Request) {
	if h.ready.Load() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte("not ready"))
}
