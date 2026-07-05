package webchat

import (
	"encoding/json"
	"net/http"
)

// handleChat streams a chat completion as Server-Sent Events.
//
// Wire format: one SSE event per [Event] in the stream. Each event is sent
// as:
//
//	data: {"type":"delta","delta":"Hello"}
//
// The frontend reads with the browser EventSource-like pattern (we use
// fetch + ReadableStream rather than EventSource because EventSource can't
// POST). On EventToolCall, the frontend prompts the user; on confirm it
// POSTs /api/tools/call, appends the assistant + tool messages, and re-POSTs
// /api/chat to continue. On EventDone or EventError the frontend finalizes.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages is required")
		return
	}

	// Stream setup. Flush headers immediately so the client starts parsing.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // bypass nginx buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Buffered channel so a slow handler doesn't block forever; size matches
	// typical chunk sizes from OpenAI-compatible APIs.
	events := make(chan Event, 32)
	done := make(chan error, 1)

	// Bridge host callbacks into SSE writes. Lives until the host's Complete
	// returns; we then close `events` to signal the writer loop to finish.
	go func() {
		err := s.host.Complete(r.Context(), CompleteRequest{
			Model:    req.Model,
			Messages: req.Messages,
			Tools:    req.Tools,
			Params:   req.Params,
		}, events)
		done <- err
		close(events)
	}()

	for ev := range events {
		data, _ := json.Marshal(ev)
		w.Write([]byte("data: "))
		w.Write(data)
		w.Write([]byte("\n\n"))
		flusher.Flush()
	}

	if err := <-done; err != nil {
		data, _ := json.Marshal(Event{Type: EventError, Error: err.Error()})
		w.Write([]byte("data: "))
		w.Write(data)
		w.Write([]byte("\n\n"))
		flusher.Flush()
	}
}
