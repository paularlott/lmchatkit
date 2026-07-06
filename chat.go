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

	// Strip UI-only fields (ID, Thinking, Info), filter out synthetic
	// info-card messages, drop empty assistant bubbles, and strip any
	// system messages (the server derives the system prompt from the
	// persona — the browser never sends system messages).
	cleaned := make([]Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Info != nil || m.Role == RoleSystem {
			continue
		}
		// Drop assistant messages with no content and no tool calls.
		if m.Role == RoleAssistant {
			content, _ := m.Content.(string)
			if content == "" && len(m.ToolCalls) == 0 {
				continue
			}
		}
		m.ID = ""
		m.Thinking = ""
		m.Info = nil
		cleaned = append(cleaned, m)
	}

	// Derive the system prompt from the persona. The persona store is
	// an in-memory cache with file watching — the lookup is a map read
	// and the prompt is always current with the persona file.
	var systemPrompt string
	if req.PersonaID != "" && s.personas != nil {
		personas, _ := s.personas.Personas(r.Context())
		for _, p := range personas {
			if p.ID == req.PersonaID {
				systemPrompt = p.SystemPrompt
				break
			}
		}
	}

	// Dynamically augment the system prompt if the host supports it.
	if a, ok := s.host.(SystemPromptAugmenter); ok {
		systemPrompt = a.AugmentSystemPrompt(r.Context(), systemPrompt)
	}

	// Prepend the system message. Always present so model templates
	// (e.g. Gemma's Jinja) see a valid conversation structure.
	req.Messages = append([]Message{{Role: RoleSystem, Content: systemPrompt}}, cleaned...)

	// Build the tool list entirely server-side. The browser doesn't
	// send tool definitions or a disabled list — it only handles the
	// approval flow (Allow / Always Allow / Deny) when tool calls come
	// back. Tool enable/disable is managed at the MCP server level.
	tools, _ := s.host.ListTools(r.Context())

	// Inject the virtual skill-retrieval tool when skill:// resources
	// exist. Server-side only — not in any user-visible tool list.
	if s.hasSkillResources(r.Context()) {
		tools = append(tools, SkillTool)
	}

	// Resolve @resource URIs and /prompt slash commands in user messages
	// server-side. The browser sends raw text; the server resolves
	// everything before forwarding to the host.
	req.Messages = s.resolveResourceRefs(r, req.Messages)
	req.Messages = s.resolveSlashCommands(r, req.Messages)

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
			Tools:    tools,
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

// slashCommandPattern matches /commandname at the start of a message.
var slashCommandPattern = regexp.MustCompile(`^/(\w[\w-]*)\s*(.*)$`)

// hasSlashCandidate reports whether any user message is a string starting with
// "/", the only case where MCP prompt resolution is needed. Used to skip the
// host ListPrompts round-trip for ordinary messages.
func hasSlashCandidate(messages []Message) bool {
	for _, m := range messages {
		if string(m.Role) != "user" {
			continue
		}
		text, ok := m.Content.(string)
		if ok && strings.HasPrefix(text, "/") {
			return true
		}
	}
	return false
}

// resolveSlashCommands scans user messages for /commandname patterns.
// If the command matches a known MCP prompt (via the host), the prompt
// is rendered server-side and the single user message is replaced with
// the prompt's returned messages. File-based commands are handled
// client-side (already expanded before sending) so they pass through.
// Unknown /commands pass through as literal text.
func (s *Server) resolveSlashCommands(r *http.Request, messages []Message) []Message {
	// Nothing to resolve unless at least one user message looks like a command.
	if !hasSlashCandidate(messages) {
		return messages
	}

	// Fetch the MCP prompt list once for this request.
	prompts, err := s.cfg.Host.ListPrompts(r.Context())
	if err != nil || len(prompts) == 0 {
		return messages
	}
	promptMap := make(map[string]bool, len(prompts))
	for _, p := range prompts {
		promptMap[p.Name] = true
	}

	resolved := make([]Message, 0, len(messages))
	for _, m := range messages {
		if string(m.Role) != "user" {
			resolved = append(resolved, m)
			continue
		}
		text, ok := m.Content.(string)
		if !ok {
			resolved = append(resolved, m)
			continue
		}

		match := slashCommandPattern.FindStringSubmatch(text)
		if match == nil {
			resolved = append(resolved, m)
			continue
		}

		name := match[1]
		argStr := match[2]

		if !promptMap[name] {
			// Not an MCP prompt — file commands are already expanded
			// by the browser. Pass through as literal text.
			resolved = append(resolved, m)
			continue
		}

		// Find the prompt declaration to parse args correctly.
		var prompt *Prompt
		for i := range prompts {
			if prompts[i].Name == name {
				prompt = &prompts[i]
				break
			}
		}

		// Parse arguments: key=value pairs, or positional for
		// single-required-arg prompts.
		args := parsePromptArgs(argStr, prompt)

		result, err := s.cfg.Host.GetPrompt(r.Context(), name, args)
		if err != nil {
			resolved = append(resolved, Message{
				Role:    "assistant",
				Content: "[error: prompt " + name + " failed: " + err.Error() + "]",
			})
			continue
		}

		// Replace the single user message with the prompt's messages.
		for _, pm := range result.Messages {
			resolved = append(resolved, Message{
				Role:    pm.Role,
				Content: pm.Content,
			})
		}
	}
	return resolved
}

// parsePromptArgs converts the argument string into a map. Supports
// key=value pairs and positional shorthand for single-required-arg prompts.
func parsePromptArgs(argStr string, prompt *Prompt) map[string]string {
	args := map[string]string{}
	if argStr == "" || prompt == nil {
		return args
	}
	parts := splitArgs(argStr)
	declared := prompt.Arguments
	required := []PromptArgument{}
	for _, a := range declared {
		if a.Required {
			required = append(required, a)
		}
	}

	if len(required) == 1 && len(parts) > 0 && !strings.Contains(parts[0], "=") {
		args[required[0].Name] = strings.Join(parts, " ")
		return args
	}

	for _, part := range parts {
		eq := strings.Index(part, "=")
		if eq > 0 {
			args[part[:eq]] = part[eq+1:]
		}
	}
	return args
}

func splitArgs(s string) []string {
	return strings.Fields(s)
}
