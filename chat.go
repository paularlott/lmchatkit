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

	encoder := json.NewEncoder(sseWriter{w: w, flusher: flusher})
	for ev := range events {
		if err := encoder.Encode(ev); err != nil {
			// Client gone or write error — give up.
			return
		}
		// SSE framing: the JSON encoder writes compact JSON + newline; we
		// prefix with "data: " and terminate with a blank line per spec.
		// We achieve that by wrapping the writer (sseWriter) which adds the
		// prefix and trailer automatically.
		flusher.Flush()
	}

	if err := <-done; err != nil {
		// Host reported a terminal error. Try to surface it to the client.
		_ = encoder.Encode(Event{Type: EventError, Error: err.Error()})
		flusher.Flush()
	}
}

// sseWriter wraps an http.ResponseWriter so each Write call produces one
// valid SSE data: line. Multiple writes for the same logical event aren't
// supported — we always emit one event per encoder.Encode call.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (s sseWriter) Write(p []byte) (int, error) {
	// Trim the trailing newline the JSON encoder adds so we can replace it
	// with the SSE-compliant terminator.
	payload := p
	for len(payload) > 0 && payload[len(payload)-1] == '\n' {
		payload = payload[:len(payload)-1]
	}
	n, err := s.w.Write([]byte("data: "))
	if err != nil {
		return n, err
	}
	m, err := s.w.Write(payload)
	if err != nil {
		return n + m, err
	}
	o, err := s.w.Write([]byte("\n\n"))
	return n + m + o, err
}
