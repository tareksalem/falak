// Package http provides HTTP transport for the Falak API, including
// hand-written SSE endpoints for streaming (Watch, Logs, Metrics)
// that grpc-gateway cannot auto-generate.
package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/api/core"
)

// SSEHandler serves Server-Sent Events for Watch and other streaming
// endpoints. Each SSE endpoint reads from a core Watch channel and
// writes events in the SSE text/event-stream format.
type SSEHandler struct {
	core   *core.Core
	logger *zap.Logger
}

// NewSSEHandler creates an SSE handler backed by the given Core.
func NewSSEHandler(c *core.Core, logger *zap.Logger) *SSEHandler {
	return &SSEHandler{core: c, logger: logger}
}

// RegisterRoutes registers all SSE streaming routes on the given mux.
func (h *SSEHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1alpha1/watch", h.handleWatch)
}

// handleWatch streams WatchEvents as SSE.
// Query params: ?resource=capsule&cluster=xxx
func (h *SSEHandler) handleWatch(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	resourceType := r.URL.Query().Get("resource")
	if resourceType == "" {
		resourceType = "capsule"
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, err := h.core.Watch(r.Context(), resourceType)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return
	}

	for ev := range ch {
		data, err := json.Marshal(map[string]interface{}{
			"type":      string(ev.Type),
			"resource":  ev.Resource,
			"timestamp": ev.Timestamp.Format(time.RFC3339),
		})
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
		flusher.Flush()
	}
}
