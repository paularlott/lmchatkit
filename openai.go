package webchat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
)

// OpenAIChatRequest builds the JSON body for an OpenAI-compatible
// /v1/chat/completions streaming request from a webchat CompleteRequest.
// Hosts that loopback to their own OpenAI endpoint (like llmrouter) can
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
