package webchat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeHost is a minimal Host used by handler tests. Methods are overridden
// per-test by setting the corresponding function field.
type fakeHost struct {
	models      []Model
	tools       []Tool
	prompts     []Prompt
	resources   []Resource
	complete    func(ctx context.Context, req CompleteRequest, events chan<- Event) error
	callTool    func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error)
	getPrompt   func(ctx context.Context, name string, args map[string]string) (PromptResult, error)
	readResource func(ctx context.Context, uri string) (ResourceResult, error)
}

func (h *fakeHost) Models(ctx context.Context) ([]Model, error) { return h.models, nil }
func (h *fakeHost) ListTools(ctx context.Context) ([]Tool, error) { return h.tools, nil }
func (h *fakeHost) ListPrompts(ctx context.Context) ([]Prompt, error) { return h.prompts, nil }
func (h *fakeHost) ListResources(ctx context.Context) ([]Resource, error) { return h.resources, nil }
func (h *fakeHost) Complete(ctx context.Context, req CompleteRequest, events chan<- Event) error {
	if h.complete == nil {
		return errors.New("Complete not stubbed")
	}
	return h.complete(ctx, req, events)
}
func (h *fakeHost) CallTool(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	if h.callTool == nil {
		return ToolResult{}, errors.New("CallTool not stubbed")
	}
	return h.callTool(ctx, name, args)
}
func (h *fakeHost) GetPrompt(ctx context.Context, name string, args map[string]string) (PromptResult, error) {
	if h.getPrompt == nil {
		return PromptResult{}, errors.New("GetPrompt not stubbed")
	}
	return h.getPrompt(ctx, name, args)
}
func (h *fakeHost) ReadResource(ctx context.Context, uri string) (ResourceResult, error) {
	if h.readResource == nil {
		return ResourceResult{}, errors.New("ReadResource not stubbed")
	}
	return h.readResource(ctx, uri)
}

// newTestServer wires a webchat Server with no on-disk persona/command dirs.
func newTestServer(t *testing.T, host Host) *Server {
	t.Helper()
	s, err := New(Config{
		Prefix: "/chat",
		Host:   host,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestHandleModels(t *testing.T) {
	host := &fakeHost{models: []Model{{ID: "m1"}, {ID: "m2"}}}
	s := newTestServer(t, host)

	req := httptest.NewRequest(http.MethodGet, "/chat/api/models", nil)
	rec := httptest.NewRecorder()
	s.handleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d (%s)", rec.Code, rec.Body.String())
	}
	var got []Model
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 || got[0].ID != "m1" {
		t.Fatalf("got %+v", got)
	}
}

func TestHandleCallTool(t *testing.T) {
	host := &fakeHost{
		callTool: func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
			if name != "lookup" {
				return ToolResult{}, errors.New("unexpected tool: " + name)
			}
			return ToolResult{Content: "result for " + string(args)}, nil
		},
	}
	s := newTestServer(t, host)

	body := `{"name":"lookup","arguments":{"q":"hi"}}`
	req := httptest.NewRequest(http.MethodPost, "/chat/api/tools/call", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleCallTool(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d (%s)", rec.Code, rec.Body.String())
	}
	var res ToolResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(res.Content, "result for") {
		t.Fatalf("unexpected content: %q", res.Content)
	}
}

func TestHandleChatStreams(t *testing.T) {
	host := &fakeHost{
		complete: func(ctx context.Context, req CompleteRequest, events chan<- Event) error {
			events <- Event{Type: EventDelta, Delta: "Hello, "}
			events <- Event{Type: EventDelta, Delta: "world!"}
			events <- Event{Type: EventDone, FinishReason: FinishStop}
			return nil
		},
	}
	s := newTestServer(t, host)

	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/chat/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type: %s", ct)
	}
	// Should contain three SSE data: lines (2 deltas + 1 done)
	payload := rec.Body.String()
	if got := strings.Count(payload, "data: "); got != 3 {
		t.Fatalf("expected 3 events, got %d (%s)", got, payload)
	}
	if !strings.Contains(payload, "Hello, ") || !strings.Contains(payload, "world!") {
		t.Fatalf("missing delta content: %s", payload)
	}
}

// TestHandleChatReturnsOnClientDisconnect verifies the streaming handler stops
// promptly when the client goes away, even if host.Complete ignores ctx and
// would otherwise block forever.
func TestHandleChatReturnsOnClientDisconnect(t *testing.T) {
	release := make(chan struct{})
	host := &fakeHost{
		complete: func(ctx context.Context, req CompleteRequest, events chan<- Event) error {
			<-release // deliberately ignore ctx to simulate a slow host
			return nil
		},
	}
	s := newTestServer(t, host)

	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/chat/api/chat", strings.NewReader(body))
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		s.handleChat(httptest.NewRecorder(), req)
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// handler returned on disconnect
	case <-time.After(2 * time.Second):
		t.Fatal("handleChat did not return after client context was cancelled")
	}
	close(release) // let the host goroutine exit
}

func TestHandleChatEmitsToolCalls(t *testing.T) {
	host := &fakeHost{
		complete: func(ctx context.Context, req CompleteRequest, events chan<- Event) error {
			events <- Event{
				Type: EventToolCall,
				ToolCall: &ToolCall{ID: "call_1", Name: "search", Arguments: json.RawMessage(`{"q":"x"}`)},
			}
			events <- Event{Type: EventDone, FinishReason: FinishToolCalls}
			return nil
		},
	}
	s := newTestServer(t, host)

	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/chat/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleChat(rec, req)

	payload := rec.Body.String()
	if !strings.Contains(payload, `"type":"tool_call"`) || !strings.Contains(payload, `"name":"search"`) {
		t.Fatalf("missing tool_call event: %s", payload)
	}
}

func TestHandleChatRejectsBadRequests(t *testing.T) {
	s := newTestServer(t, &fakeHost{})

	cases := []struct {
		name string
		body string
		want int
	}{
		{"not json", `not-json`, http.StatusBadRequest},
		{"missing model", `{"messages":[]}`, http.StatusBadRequest},
		{"empty messages", `{"model":"m"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/chat/api/chat", strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			s.handleChat(rec, req)
			if rec.Code != c.want {
				t.Fatalf("want %d got %d (%s)", c.want, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestMountRegistersRoutes(t *testing.T) {
	s := newTestServer(t, &fakeHost{
		models: []Model{{ID: "m"}},
		complete: func(ctx context.Context, req CompleteRequest, events chan<- Event) error {
			events <- Event{Type: EventDone, FinishReason: FinishStop}
			return nil
		},
	})
	mux := http.NewServeMux()
	s.Mount(mux)

	// webchat owns API + assets routes. The host owns the page itself
	// (so it lives in the host's template tree where Tailwind can scan
	// it); we deliberately do NOT register /chat here.
	for _, p := range []string{
		"/chat/api/personas",
		"/chat/api/commands",
		"/chat/api/models",
		"/chat/api/chat",
		"/chat/api/tools/call",
		"/chat/api/prompts",
		"/chat/api/prompts/get",
		"/chat/api/resources",
		"/chat/api/resources/read",
		"/chat/assets/chat.js",
	} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler registered for %s", p)
		}
	}
}

func TestAssetHandlerServesJS(t *testing.T) {
	s := newTestServer(t, &fakeHost{})
	req := httptest.NewRequest(http.MethodGet, "/chat/assets/chat.js", nil)
	rec := httptest.NewRecorder()
	s.handleAsset(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("empty body")
	}
	if !strings.Contains(rec.Body.String(), "processMarkdown") {
		t.Fatalf("expected bundled chat.js to include the markdown processor")
	}
}

// min is a tiny helper for the substring test (Go 1.21+ has builtin min, but
// we keep this for clarity when slicing).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Ensure unused io import stays referenced.
var _ = io.EOF
