package webchat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	mcplib "github.com/paularlott/mcp"
)

// use this instead of hand-rolling the conversion. The returned map is
// ready to json.Marshal and POST.
func OpenAIChatRequest(req CompleteRequest) map[string]interface{} {
	messages := make([]map[string]interface{}, 0, len(req.Messages))
	for _, m := range req.Messages {
		msg := map[string]interface{}{"role": string(m.Role)}
		if m.Content != "" {
			msg["content"] = m.Content
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]interface{}, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				calls = append(calls, map[string]interface{}{
					"id":       c.ID,
					"type":     "function",
					"function": map[string]interface{}{"name": c.Name, "arguments": string(c.Arguments)},
				})
			}
			msg["tool_calls"] = calls
		}
		messages = append(messages, msg)
	}

	body := map[string]interface{}{
		"model":    req.Model,
		"messages": messages,
		"stream":   true,
	}

	if len(req.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]interface{}{"type": "object"}
			}
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  schema,
				},
			})
		}
		body["tools"] = tools
	}

	for k, v := range req.Params {
		body[k] = v
	}

	return body
}

// TranslateOpenAIStream reads an OpenAI-compatible SSE stream (the response
// body from POST /v1/chat/completions with stream:true) and emits webchat
// events onto the events channel. Handles:
//   - delta.content → EventDelta
//   - delta.reasoning_content / delta.reasoning → EventReasoning
//   - delta.tool_calls (fragmented by index) → accumulated and flushed as
//     EventToolCall before the terminal event
//   - finish_reason → mapped to FinishStop / FinishToolCalls / FinishLength
//   - data: [DONE] → stream end
//
// Hosts that loopback to their own OpenAI endpoint can call this directly
// instead of reimplementing the SSE parsing. The events channel must be
// buffered (webchat's chat handler uses a 32-slot buffer).
func TranslateOpenAIStream(ctx context.Context, body io.Reader, events chan<- Event) error {
	type toolCallAccum struct {
		ID        string
		Name      string
		Arguments strings.Builder
	}
	accums := map[int]*toolCallAccum{}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tool-call args can get large
	var finalReason string

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			continue
		}

		for _, choice := range chunk.Choices {
			// Reasoning content (o1-style reasoning_content, or generic
			// "reasoning" field used by some open-source models).
			if choice.Delta.ReasoningContent != "" {
				if err := emit(ctx, events, Event{Type: EventReasoning, Reasoning: choice.Delta.ReasoningContent}); err != nil {
					return err
				}
			}
			if choice.Delta.Reasoning != "" {
				if err := emit(ctx, events, Event{Type: EventReasoning, Reasoning: choice.Delta.Reasoning}); err != nil {
					return err
				}
			}
			// Visible content.
			if choice.Delta.Content != "" {
				if err := emit(ctx, events, Event{Type: EventDelta, Delta: choice.Delta.Content}); err != nil {
					return err
				}
			}
			// Tool calls arrive fragmented by index — accumulate.
			for _, tc := range choice.Delta.ToolCalls {
				accum, ok := accums[tc.Index]
				if !ok {
					accum = &toolCallAccum{}
					accums[tc.Index] = accum
				}
				if tc.ID != "" {
					accum.ID = tc.ID
				}
				if tc.Function.Name != "" {
					accum.Name = tc.Function.Name
				}
				accum.Arguments.WriteString(tc.Function.Arguments)
			}
			if choice.FinishReason != nil {
				finalReason = *choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	// Map OpenAI finish_reason → webchat FinishReason.
	finish := FinishStop
	switch finalReason {
	case "tool_calls":
		finish = FinishToolCalls
	case "length":
		finish = FinishLength
	}

	// Flush accumulated tool calls in index order before the terminal event.
	if len(accums) > 0 {
		indices := make([]int, 0, len(accums))
		for i := range accums {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		for _, idx := range indices {
			a := accums[idx]
			if err := emit(ctx, events, Event{
				Type: EventToolCall,
				ToolCall: &ToolCall{
					ID:        a.ID,
					Name:      a.Name,
					Arguments: json.RawMessage(a.Arguments.String()),
				},
			}); err != nil {
				return err
			}
		}
	}

	return emit(ctx, events, Event{Type: EventDone, FinishReason: finish})
}

// emit sends an event on the channel, respecting context cancellation.
func emit(ctx context.Context, events chan<- Event, ev Event) error {
	select {
	case events <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
// StandardHost is a ready-to-use [Host] implementation for apps that:
//   - Talk to an OpenAI-compatible /v1/chat/completions endpoint
//   - Expose MCP tools/prompts/resources via an *mcp.Server
//
// Hosts with different needs (non-OpenAI LLM, custom tool backends, per-user
// MCP servers) construct a StandardHost with the appropriate functions. Hosts
// with fundamentally different architectures implement [Host] themselves.
//
// Auth is NOT handled here — the host wraps the webchat routes with its own
// middleware via [Config.AuthMiddleware], exactly like it protects any other
// route. StandardHost never sees tokens, sessions, or passwords.
type StandardHost struct {
	// ModelsFunc returns the models available for selection. Called on each
	// /api/models request so the list is always current. Required.
	ModelsFunc func(ctx context.Context) ([]Model, error)

	// OpenAIBaseURL is where /v1/chat/completions lives. For a self-loopback
	// (llmrouter), this is "http://127.0.0.1:<port>". For an external LLM
	// proxy, it's that proxy's URL. Required.
	OpenAIBaseURL string

	// OpenAIToken is the bearer token sent with the completion request.
	// For a self-loopback this is the server's API token; for a user-scoped
	// proxy it's the user's token. May be empty if the endpoint doesn't
	// require auth.
	OpenAIToken string

	// MCPServer returns the *mcp.Server to use for tool/prompt/resource
	// calls. For single-user hosts (llmrouter), return the same server
	// every time. For per-user hosts (knot), extract the user from the
	// context and return their server. Return nil to disable MCP for that
	// request.
	MCPServer func(ctx context.Context) *mcplib.Server

	// HTTPClient overrides the default client. nil = use http.DefaultClient
	// with no timeout (streaming needs no overall timeout).
	HTTPClient *http.Client
}

// Compile-time check.
var _ Host = (*StandardHost)(nil)

func (h *StandardHost) Models(ctx context.Context) ([]Model, error) {
	return h.ModelsFunc(ctx)
}

func (h *StandardHost) Complete(ctx context.Context, req CompleteRequest, events chan<- Event) error {
	body := OpenAIChatRequest(req)
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.OpenAIBaseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if h.OpenAIToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+h.OpenAIToken)
	}

	client := h.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upstream returned %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}

	return TranslateOpenAIStream(ctx, resp.Body, events)
}

func (h *StandardHost) ListTools(ctx context.Context) ([]Tool, error) {
	srv := h.mcpServer(ctx)
	if srv == nil {
		return nil, nil
	}
	tools := srv.ListToolsWithContext(ctx)
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		var schema map[string]interface{}
		if s, ok := t.InputSchema.(map[string]interface{}); ok {
			schema = s
		} else if t.InputSchema != nil {
			if b, err := json.Marshal(t.InputSchema); err == nil {
				_ = json.Unmarshal(b, &schema)
			}
		}
		out = append(out, Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return out, nil
}

func (h *StandardHost) CallTool(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, error) {
	srv := h.mcpServer(ctx)
	if srv == nil {
		return ToolResult{}, fmt.Errorf("MCP server not available")
	}
	var argsMap map[string]any
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &argsMap); err != nil {
			return ToolResult{}, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}
	resp, err := srv.CallTool(ctx, name, argsMap)
	if err != nil {
		if isMCPToolError(err) {
			return ToolResult{Content: err.Error(), IsError: true}, nil
		}
		return ToolResult{}, err
	}
	return ToolResult{Content: mcpToolResponseText(resp)}, nil
}

func (h *StandardHost) ListPrompts(ctx context.Context) ([]Prompt, error) {
	srv := h.mcpServer(ctx)
	if srv == nil {
		return nil, nil
	}
	prompts := srv.ListPrompts(ctx)
	out := make([]Prompt, 0, len(prompts))
	for _, p := range prompts {
		info := Prompt{Name: p.Name, Description: p.Description}
		for _, a := range p.Arguments {
			info.Arguments = append(info.Arguments, PromptArgument{
				Name: a.Name, Description: a.Description, Required: a.Required,
			})
		}
		out = append(out, info)
	}
	return out, nil
}

func (h *StandardHost) GetPrompt(ctx context.Context, name string, args map[string]string) (PromptResult, error) {
	srv := h.mcpServer(ctx)
	if srv == nil {
		return PromptResult{}, fmt.Errorf("MCP server not available")
	}
	resp, err := srv.GetPrompt(ctx, name, args)
	if err != nil {
		return PromptResult{}, err
	}
	out := PromptResult{Description: resp.Description}
	for _, m := range resp.Messages {
		out.Messages = append(out.Messages, PromptMessage{
			Role: Role(m.Role), Content: m.Content.Text,
		})
	}
	return out, nil
}

func (h *StandardHost) ListResources(ctx context.Context) ([]Resource, error) {
	srv := h.mcpServer(ctx)
	if srv == nil {
		return nil, nil
	}
	resources := srv.ListResources(ctx)
	templates := srv.ListResourceTemplates(ctx)
	out := make([]Resource, 0, len(resources)+len(templates))
	for _, r := range resources {
		out = append(out, Resource{
			URI: r.URI, Name: r.Name, Description: r.Description, MimeType: r.MimeType,
		})
	}
	for _, t := range templates {
		out = append(out, Resource{
			URI: t.URITemplate, Template: true, Name: t.Name, Description: t.Description, MimeType: t.MimeType,
		})
	}
	return out, nil
}

func (h *StandardHost) ReadResource(ctx context.Context, uri string) (ResourceResult, error) {
	srv := h.mcpServer(ctx)
	if srv == nil {
		return ResourceResult{}, fmt.Errorf("MCP server not available")
	}
	resp, err := srv.ReadResource(ctx, uri)
	if err != nil {
		return ResourceResult{}, err
	}
	out := ResourceResult{URI: uri}
	if len(resp.Contents) > 0 {
		out.Text = resp.Contents[0].Text
		out.Blob = resp.Contents[0].Blob
		out.MimeType = resp.Contents[0].MimeType
	}
	return out, nil
}

// mcpServer resolves the MCP server for this request. Returns nil if no
// MCPServer function was configured.
func (h *StandardHost) mcpServer(ctx context.Context) *mcplib.Server {
	if h.MCPServer == nil {
		return nil
	}
	return h.MCPServer(ctx)
}

// isMCPToolError reports whether err is an MCP *ToolError (any code). The
// host treats tool errors as successful calls with an error payload so the
// model can react to the failure rather than seeing a transport error.
func isMCPToolError(err error) bool {
	type toolErr interface{ Code() int }
	_, ok := err.(toolErr)
	return ok
}

// mcpToolResponseText flattens an MCP tool response into a single string
// the model can consume. Multi-content responses concatenate text parts
// with newlines; non-text parts get a placeholder.
func mcpToolResponseText(r *mcplib.ToolResponse) string {
	if r == nil || len(r.Content) == 0 {
		return ""
	}
	var b strings.Builder
	for i, c := range r.Content {
		if i > 0 {
			b.WriteString("\n")
		}
		switch {
		case c.Text != "":
			b.WriteString(c.Text)
		case c.Data != "":
			b.WriteString("[" + c.Type + ":" + fmt.Sprintf("%d", len(c.Data)) + " bytes]")
		default:
			b.WriteString("[" + c.Type + "]")
		}
	}
	return b.String()
}
