package webchat

import (
	"encoding/json"
	"net/http"
)

// handleEvents is the SSE endpoint at GET /api/events. Clients connect
// once (one per browser tab) and receive push notifications for:
//   - conversation_saved / conversation_deleted (cross-tab sync)
//   - tools_changed / prompts_changed / resources_changed (scriptling
//     watcher detected file changes — replaces fetch-on-done)
//
// The connection stays open until the client disconnects or the server
// shuts down. EventSource in the browser handles auto-reconnect.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.cfg.Events.Subscribe()
	defer s.cfg.Events.Unsubscribe(ch)

	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(event)
			w.Write([]byte("data: "))
			w.Write(data)
			w.Write([]byte("\n\n"))
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
