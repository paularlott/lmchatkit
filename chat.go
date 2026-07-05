package webchat

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// atResourcePattern matches @scheme://value inside message text. The
// server resolves each match to actual resource content before forwarding
// to the LLM — the browser never fetches resource payloads.
var atResourcePattern = regexp.MustCompile(`@(\w+://[^\s@]+)`)

// handleChat streams a chat completion as Server-Sent Events.
//
// Before forwarding to the host, the server resolves any @scheme://value
// patterns in user messages by calling the host's ReadResource. The
// resource content becomes a content-block array (text for text
// resources, image_url for images). The @uri stays in the original
// message text for the transcript; the LLM sees the resolved content.
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

	// Resolve @resource URIs in user messages server-side. Each @uri
	// is replaced with a content block (text or image_url). The LLM
	// sees the resource content; the transcript keeps the @uri text.
	req.Messages = s.resolveResourceRefs(r, req.Messages)

	// Stream setup.
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

	events := make(chan Event, 32)
	done := make(chan error, 1)

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

// resolveResourceRefs scans user messages for @scheme://value patterns
// and replaces them with content blocks. Resources are resolved first
// (they're context), then the user's message text follows as the final
// block — regardless of where @uri appeared in the original text. If a
// message has no @refs, it passes through unchanged (string content).
// Non-user messages pass through untouched.
func (s *Server) resolveResourceRefs(r *http.Request, messages []Message) []Message {
	for i := range messages {
		m := &messages[i]
		if string(m.Role) != "user" {
			continue
		}
		text, ok := m.Content.(string)
		if !ok {
			continue
		}
		matches := atResourcePattern.FindAllStringSubmatch(text, -1)
		if len(matches) == 0 {
			continue
		}

		// Collect resource content blocks first (context precedes the
		// question), then the user's cleaned message text last.
		var blocks []map[string]interface{}

		for _, match := range matches {
			uri := match[1]

			result, err := s.cfg.Host.ReadResource(r.Context(), uri)
			if err != nil {
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": "[error: could not read resource " + uri + ": " + err.Error() + "]",
				})
				continue
			}

			if strings.HasPrefix(result.MimeType, "image/") && result.Blob != "" {
				blocks = append(blocks, map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]interface{}{
						"url": "data:" + result.MimeType + ";base64," + result.Blob,
					},
				})
			} else if result.Text != "" {
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": "Resource " + uri + ":\n" + result.Text,
				})
			}
		}

		// Strip all @uri references from the text, leaving the user's
		// actual message. This goes last so the model sees resources
		// as context, then the question.
		cleaned := strings.TrimSpace(atResourcePattern.ReplaceAllString(text, ""))
		if cleaned != "" {
			blocks = append(blocks, map[string]interface{}{
				"type": "text",
				"text": cleaned,
			})
		}

		if len(blocks) > 0 {
			m.Content = blocks
		}
	}
	return messages
}
