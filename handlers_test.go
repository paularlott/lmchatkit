package lmchatkit

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
	models       []Model
	tools        []Tool
	listToolsErr error
	prompts      []Prompt
	resources    []Resource
	complete     func(ctx context.Context, req CompleteRequest, events chan<- Event) error
	callTool     func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error)
	getPrompt    func(ctx context.Context, name string, args map[string]string) (PromptResult, error)
	readResource func(ctx context.Context, uri string) (ResourceResult, error)
}

func (h *fakeHost) Models(ctx context.Context) ([]Model, error) { return h.models, nil }
func (h *fakeHost) ListTools(ctx context.Context) ([]Tool, error) {
	if h.listToolsErr != nil {
		return nil, h.listToolsErr
	}
	return h.tools, nil
}
func (h *fakeHost) ListPrompts(ctx context.Context) ([]Prompt, error)     { return h.prompts, nil }
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

// fakeScopedHost wraps fakeHost and additionally implements
// [SourceScopedHost], for tests exercising cross-server tool/resource
// ownership enforcement (toolSourceAllows, handleReadResource's Via path).
type fakeScopedHost struct {
	*fakeHost
	toolSource             func(ctx context.Context, name string) (string, bool)
	readResourceFromSource func(ctx context.Context, source, uri string) (ResourceResult, error)
}

func (h *fakeScopedHost) ToolSource(ctx context.Context, name string) (string, bool) {
	return h.toolSource(ctx, name)
}

func (h *fakeScopedHost) ReadResourceFromSource(ctx context.Context, source, uri string) (ResourceResult, error) {
	return h.readResourceFromSource(ctx, source, uri)
}

var _ SourceScopedHost = (*fakeScopedHost)(nil)

// fakeAllToolsHost wraps fakeHost and additionally implements
// [AllToolsHost], for tests exercising the discoverable-tool visibility gap:
// allTools includes tools that ListTools (tools) omits, mirroring a
// discoverable MCP tool that's hidden from the model-facing list but still
// directly callable by an app view.
type fakeAllToolsHost struct {
	*fakeHost
	allTools []Tool
}

func (h *fakeAllToolsHost) ListAllTools(ctx context.Context) ([]Tool, error) {
	return h.allTools, nil
}

var _ AllToolsHost = (*fakeAllToolsHost)(nil)

// newTestServer wires a lmchatkit Server with no on-disk persona/command dirs.
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
		tools: []Tool{{Name: "lookup"}},
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

// TestHandleCallTool_EnforcesVisibility is the regression test for the MCP
// Apps visibility hole: without it, a model-approved call could reach an
// app-only ("app"-only visibility) tool, and — worse — a call proxied from
// a mounted view (source: "app") could reach ANY tool regardless of
// visibility, including one meant only for the model. Both directions must
// be rejected; each direction's legitimate case must still succeed.
func TestHandleCallTool_EnforcesVisibility(t *testing.T) {
	host := &fakeHost{
		tools: []Tool{
			{Name: "add_sale", Visibility: []string{"app"}},
			{Name: "sales_report", Visibility: []string{"model"}},
			{Name: "ping"}, // no visibility set = both, per spec default
		},
		callTool: func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
			return ToolResult{Content: "ok:" + name}, nil
		},
	}
	s := newTestServer(t, host)

	call := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/chat/api/tools/call", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleCallTool(rec, req)
		return rec
	}

	t.Run("model path cannot reach an app-only tool", func(t *testing.T) {
		rec := call(t, `{"name":"add_sale","arguments":{}}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("app path cannot reach a model-only tool", func(t *testing.T) {
		rec := call(t, `{"name":"sales_report","arguments":{},"source":"app"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("app path CAN reach an app-only tool", func(t *testing.T) {
		rec := call(t, `{"name":"add_sale","arguments":{},"source":"app"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("model path CAN reach a model-visible tool", func(t *testing.T) {
		rec := call(t, `{"name":"sales_report","arguments":{}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("default (no visibility set) is callable from both", func(t *testing.T) {
		if rec := call(t, `{"name":"ping","arguments":{}}`); rec.Code != http.StatusOK {
			t.Errorf("model path: status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if rec := call(t, `{"name":"ping","arguments":{},"source":"app"}`); rec.Code != http.StatusOK {
			t.Errorf("app path: status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a name unknown to ListTools passes through to Host.CallTool", func(t *testing.T) {
		// lmchatkit__get_skill is special-cased by trySkillToolCall for the
		// model role only (see the next subtest for the app role), so on the
		// model path this answers from the intercept without ever consulting
		// toolVisibilityAllows.
		rec := call(t, `{"name":"lmchatkit__get_skill","arguments":{}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (model path answers from the skill intercept): %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an app view cannot pull host-level skills via the skill tool", func(t *testing.T) {
		// The intercept is model-role only; for role "app" the name falls
		// through to toolVisibilityAllows, which doesn't know the virtual
		// skill tool (it isn't in Host.ListTools) and fails closed — a
		// sandboxed view must not be able to read host skill:// content,
		// which would cross the very server boundary SourceScopedHost
		// enforces for real tools.
		rec := call(t, `{"name":"lmchatkit__get_skill","arguments":{"uri":"skill://anything"},"source":"app","via":"sales_report"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unrecognized name (not the skill tool) is denied, not let through", func(t *testing.T) {
		// Regression test: toolVisibilityAllows used to fail OPEN both on a
		// ListTools error and on a name it didn't recognize. Both must now
		// fail closed.
		rec := call(t, `{"name":"totally_unknown_tool","arguments":{}}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a ListTools error denies rather than allows", func(t *testing.T) {
		errHost := &fakeHost{
			listToolsErr: errors.New("boom"),
			callTool: func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
				return ToolResult{Content: "ok:" + name}, nil
			},
		}
		errServer := newTestServer(t, errHost)
		req := httptest.NewRequest(http.MethodPost, "/chat/api/tools/call", strings.NewReader(`{"name":"ping","arguments":{}}`))
		rec := httptest.NewRecorder()
		errServer.handleCallTool(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestHandleCallTool_AppCanReachDiscoverableTool reproduces a real bug: a
// discoverable (search-only) MCP tool is excluded from Host.ListTools'
// model-facing result by design, but an app view calls it directly by its
// real name (chat.js's resolveAppToolName), not through execute_tool. Using
// plain ListTools for the visibility check made toolVisibilityAllows unable
// to find the tool at all, denying every app-initiated call to it. A host
// that also implements AllToolsHost must be consulted instead so the tool's
// _meta.ui.visibility can be found.
func TestHandleCallTool_AppCanReachDiscoverableTool(t *testing.T) {
	host := &fakeAllToolsHost{
		fakeHost: &fakeHost{
			// Deliberately empty: this is what ListTools returns for a
			// discoverable tool — absent, same as the model-facing list.
			tools: []Tool{},
			callTool: func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
				return ToolResult{Content: "ok:" + name}, nil
			},
		},
		allTools: []Tool{
			{Name: "memory__spin_wheel", Visibility: []string{"model", "app"}},
		},
	}
	s := newTestServer(t, host)

	req := httptest.NewRequest(http.MethodPost, "/chat/api/tools/call", strings.NewReader(
		`{"name":"memory__spin_wheel","arguments":{},"source":"app"}`))
	rec := httptest.NewRecorder()
	s.handleCallTool(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleCallTool_EnforcesSourceOwnership is the regression test for the
// namespace-collision gap: when two federated servers connect without name
// prefixes, an "app"-visible tool on one server must not be reachable from
// a view mounted by a tool on a *different* server, even though visibility
// alone would allow it. Requires the host to implement [SourceScopedHost];
// a host that doesn't is exercised separately below (falls through as
// allowed, matching TestHandleCallTool_EnforcesVisibility's plain fakeHost).
func TestHandleCallTool_EnforcesSourceOwnership(t *testing.T) {
	// Two tools, both "app"-visible, "owned" by two different (fake) remote
	// servers "server-a" and "server-b" — same shape a namespace-less
	// federation collision would produce.
	sources := map[string]string{
		"mount_tool_a": "server-a",
		"action_a":     "server-a",
		"action_b":     "server-b",
	}
	base := &fakeHost{
		tools: []Tool{
			{Name: "mount_tool_a", Visibility: []string{"app"}},
			{Name: "action_a", Visibility: []string{"app"}},
			{Name: "action_b", Visibility: []string{"app"}},
		},
		callTool: func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
			return ToolResult{Content: "ok:" + name}, nil
		},
	}
	host := &fakeScopedHost{
		fakeHost: base,
		toolSource: func(ctx context.Context, name string) (string, bool) {
			src, ok := sources[name]
			return src, ok
		},
	}
	s := newTestServer(t, host)

	call := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/chat/api/tools/call", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleCallTool(rec, req)
		return rec
	}

	t.Run("a view can call a tool on its own server", func(t *testing.T) {
		rec := call(t, `{"name":"action_a","arguments":{},"source":"app","via":"mount_tool_a"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a view cannot call a same-visibility tool on a different server", func(t *testing.T) {
		rec := call(t, `{"name":"action_b","arguments":{},"source":"app","via":"mount_tool_a"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unresolvable via denies rather than allows", func(t *testing.T) {
		rec := call(t, `{"name":"action_a","arguments":{},"source":"app","via":"unknown_mount_tool"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the model path (not source:app) is not source-checked", func(t *testing.T) {
		// action_a/action_b are app-only, so this is denied by visibility —
		// but via an unrelated dimension of the check, confirming source
		// enforcement only kicks in for role=="app".
		rec := call(t, `{"name":"action_a","arguments":{}}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (visibility, not source): %s", rec.Code, rec.Body.String())
		}
	})
}

// TestHandleCallTool_SourceOwnership_UnscopedHostAllows confirms a Host
// that doesn't implement SourceScopedHost (a single, non-federated MCP
// server has no cross-server ambiguity) isn't newly broken by the
// ownership check — it's a no-op allow, same as before this check existed.
func TestHandleCallTool_SourceOwnership_UnscopedHostAllows(t *testing.T) {
	host := &fakeHost{
		tools: []Tool{
			{Name: "mount_tool", Visibility: []string{"app"}},
			{Name: "action", Visibility: []string{"app"}},
		},
		callTool: func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
			return ToolResult{Content: "ok:" + name}, nil
		},
	}
	s := newTestServer(t, host)
	req := httptest.NewRequest(http.MethodPost, "/chat/api/tools/call", strings.NewReader(`{"name":"action","arguments":{},"source":"app","via":"mount_tool"}`))
	rec := httptest.NewRecorder()
	s.handleCallTool(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleReadResource_EnforcesSourceOwnership is the resource-side twin
// of TestHandleCallTool_EnforcesSourceOwnership: a resources/read proxied
// from a mounted view must be scoped to the mounting tool's own server.
func TestHandleReadResource_EnforcesSourceOwnership(t *testing.T) {
	sources := map[string]string{"mount_tool_a": "server-a"}
	base := &fakeHost{}
	host := &fakeScopedHost{
		fakeHost: base,
		toolSource: func(ctx context.Context, name string) (string, bool) {
			src, ok := sources[name]
			return src, ok
		},
		readResourceFromSource: func(ctx context.Context, source, uri string) (ResourceResult, error) {
			if source != "server-a" {
				return ResourceResult{}, errors.New("unexpected source: " + source)
			}
			return ResourceResult{URI: uri, Text: "content from server-a"}, nil
		},
	}
	s := newTestServer(t, host)

	call := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/chat/api/resources/read", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleReadResource(rec, req)
		return rec
	}

	t.Run("a resource scoped to the mounting tool's server is readable", func(t *testing.T) {
		rec := call(t, `{"uri":"ui://server-a/widget","via":"mount_tool_a"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var res ResourceResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if res.Text != "content from server-a" {
			t.Fatalf("unexpected text: %q", res.Text)
		}
	})

	t.Run("an unresolvable via denies rather than reading unrestricted", func(t *testing.T) {
		rec := call(t, `{"uri":"ui://server-b/widget","via":"unknown_mount_tool"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestHandleReadResource_UnscopedOrNoVia confirms the pre-existing,
// unrestricted read still works: a Host without SourceScopedHost, or a
// request with no Via at all (the host's own trusted call sites always set
// it, but the field is optional for any other caller).
func TestHandleReadResource_UnscopedOrNoVia(t *testing.T) {
	host := &fakeHost{
		readResource: func(ctx context.Context, uri string) (ResourceResult, error) {
			return ResourceResult{URI: uri, Text: "unrestricted"}, nil
		},
	}
	s := newTestServer(t, host)
	req := httptest.NewRequest(http.MethodPost, "/chat/api/resources/read", strings.NewReader(`{"uri":"ui://anything"}`))
	rec := httptest.NewRecorder()
	s.handleReadResource(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
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
				Type:     EventToolCall,
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

// TestHandleChatAlwaysInjectsSystemPrompt verifies the server always
// prepends a system message with non-blank content: the active persona's
// prompt when set, otherwise a sane default. A blank or whitespace-only
// persona prompt must fall back to the default — some providers (e.g.
// Gemma in LM Studio) reject a content-less system message.
func TestHandleChatAlwaysInjectsSystemPrompt(t *testing.T) {
	cases := []struct {
		name        string
		personaSrc  PersonaSource
		bodyPersona string
		wantPrompt  string
	}{
		{
			name:       "no persona defaults to helpful assistant",
			personaSrc: nil,
			wantPrompt: "You are a helpful assistant.",
		},
		{
			name: "persona prompt is used",
			personaSrc: StaticPersonas{
				{ID: "klingon", Name: "Klingon", SystemPrompt: "Behave like a klingon."},
			},
			bodyPersona: "klingon",
			wantPrompt:  "Behave like a klingon.",
		},
		{
			name: "blank persona prompt defaults",
			personaSrc: StaticPersonas{
				{ID: "empty", Name: "Empty", SystemPrompt: ""},
			},
			bodyPersona: "empty",
			wantPrompt:  "You are a helpful assistant.",
		},
		{
			name: "whitespace-only persona prompt defaults",
			personaSrc: StaticPersonas{
				{ID: "spaces", Name: "Spaces", SystemPrompt: "   \t\n  "},
			},
			bodyPersona: "spaces",
			wantPrompt:  "You are a helpful assistant.",
		},
		{
			name: "unknown persona id defaults",
			personaSrc: StaticPersonas{
				{ID: "klingon", Name: "Klingon", SystemPrompt: "Behave like a klingon."},
			},
			bodyPersona: "does-not-exist",
			wantPrompt:  "You are a helpful assistant.",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got CompleteRequest
			host := &fakeHost{
				complete: func(ctx context.Context, req CompleteRequest, events chan<- Event) error {
					got = req
					events <- Event{Type: EventDone, FinishReason: FinishStop}
					return nil
				},
			}
			cfg := Config{Prefix: "/chat", Host: host}
			if c.personaSrc != nil {
				cfg.PersonaSource = c.personaSrc
			}
			s, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			body := `{"model":"m","persona_id":"` + c.bodyPersona + `","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest(http.MethodPost, "/chat/api/chat", strings.NewReader(body))
			rec := httptest.NewRecorder()
			s.handleChat(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status: %d (%s)", rec.Code, rec.Body.String())
			}
			if len(got.Messages) == 0 || got.Messages[0].Role != RoleSystem {
				t.Fatalf("expected leading system message, got %+v", got.Messages)
			}
			content, _ := got.Messages[0].Content.(string)
			if content != c.wantPrompt {
				t.Fatalf("system prompt: want %q got %q", c.wantPrompt, content)
			}
		})
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

	// lmchatkit owns API + assets routes. The host owns the page itself
	// (so it lives in the host's template tree where Tailwind can scan
	// it); we deliberately do NOT register /chat here.
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/chat/api/personas"},
		{http.MethodGet, "/chat/api/commands"},
		{http.MethodGet, "/chat/api/models"},
		{http.MethodPost, "/chat/api/chat"},
		{http.MethodPost, "/chat/api/tools/call"},
		{http.MethodGet, "/chat/api/prompts"},
		{http.MethodPost, "/chat/api/prompts/get"},
		{http.MethodGet, "/chat/api/resources"},
		{http.MethodPost, "/chat/api/resources/read"},
		{http.MethodPost, "/chat/api/app-proxy"},
		{http.MethodGet, "/chat/assets/chat.js"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler registered for %s %s", tc.method, tc.path)
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

func TestAssetHandlerETagRevalidation(t *testing.T) {
	s := newTestServer(t, &fakeHost{})

	// First fetch: 200 with a content-hash ETag and a revalidate cache policy.
	req := httptest.NewRequest(http.MethodGet, "/chat/assets/chat.js", nil)
	rec := httptest.NewRecorder()
	s.handleAsset(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first fetch status %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag on first fetch")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "must-revalidate") {
		t.Fatalf("Cache-Control %q does not force revalidation", cc)
	}

	// Matching If-None-Match → 304 with no body.
	req2 := httptest.NewRequest(http.MethodGet, "/chat/assets/chat.js", nil)
	req2.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	s.handleAsset(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("revalidation status %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("304 should have no body, got %d bytes", rec2.Body.Len())
	}

	// Mismatched ETag → 200 with the body again (the upgrade case).
	req3 := httptest.NewRequest(http.MethodGet, "/chat/assets/chat.js", nil)
	req3.Header.Set("If-None-Match", `"deadbeef"`)
	rec3 := httptest.NewRecorder()
	s.handleAsset(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("mismatched-etag status %d, want 200", rec3.Code)
	}
	if rec3.Body.Len() == 0 {
		t.Fatal("expected body on etag mismatch")
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

// TestHandleAppProxy_ToolsCall covers the app-proxy transport's tool-call
// relay: a mounted view's bare, host-agnostic tool name is dispatched to the
// mounting tool's namespace server-side (replacing the prefix computation
// chat.js used to do client-side via resolveAppToolName), the client echo
// of ToolResult.Server is only a cross-check, and every cross-server or
// visibility escape fails closed.
func TestHandleAppProxy_ToolsCall(t *testing.T) {
	sources := map[string]string{
		// Federated tools carry their namespace in the host-side name;
		// native tools have no namespace and a "" source.
		"ns1__mount":      "server-a",
		"ns1__action":     "server-a",
		"ns1__model_only": "server-a",
		"ns2__action":     "server-b",
		"native_mount":    "",
		"native_action":   "",
	}
	dispatched := ""
	base := &fakeHost{
		tools: []Tool{
			{Name: "ns1__mount", Visibility: []string{"app"}},
			{Name: "ns1__action", Visibility: []string{"app"}},
			{Name: "ns1__model_only", Visibility: []string{"model"}},
			{Name: "ns2__action", Visibility: []string{"app"}},
			{Name: "native_mount", Visibility: []string{"app"}},
			{Name: "native_action", Visibility: []string{"app"}},
		},
		callTool: func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
			dispatched = name
			return ToolResult{Content: "ok:" + name}, nil
		},
	}
	host := &fakeScopedHost{
		fakeHost: base,
		toolSource: func(ctx context.Context, name string) (string, bool) {
			src, ok := sources[name]
			return src, ok
		},
	}
	s := newTestServer(t, host)

	call := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		dispatched = ""
		req := httptest.NewRequest(http.MethodPost, "/chat/api/app-proxy", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleAppProxy(rec, req)
		return rec
	}

	t.Run("a bare name is dispatched to the mounting tool's namespace", func(t *testing.T) {
		rec := call(t, `{"method":"tools/call","params":{"name":"action","arguments":{}},"via":"ns1__mount"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if dispatched != "ns1__action" {
			t.Errorf("dispatched = %q, want %q (own server, not the same-named tool on server-b)", dispatched, "ns1__action")
		}
	})

	t.Run("an already-namespaced name is dispatched verbatim", func(t *testing.T) {
		rec := call(t, `{"method":"tools/call","params":{"name":"ns1__action","arguments":{}},"via":"ns1__mount"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if dispatched != "ns1__action" {
			t.Errorf("dispatched = %q, want %q", dispatched, "ns1__action")
		}
	})

	t.Run("a cross-server tool is not reachable even by its full name", func(t *testing.T) {
		rec := call(t, `{"method":"tools/call","params":{"name":"ns2__action","arguments":{}},"via":"ns1__mount"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (source mismatch): %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a model-only tool stays unreachable from a view", func(t *testing.T) {
		rec := call(t, `{"method":"tools/call","params":{"name":"model_only","arguments":{}},"via":"ns1__mount"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (visibility): %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unknown tool name fails closed", func(t *testing.T) {
		rec := call(t, `{"method":"tools/call","params":{"name":"nope","arguments":{}},"via":"ns1__mount"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a native mount dispatches bare names unchanged", func(t *testing.T) {
		rec := call(t, `{"method":"tools/call","params":{"name":"native_action","arguments":{}},"via":"native_mount"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if dispatched != "native_action" {
			t.Errorf("dispatched = %q, want %q", dispatched, "native_action")
		}
	})

	t.Run("a native mount cannot reach a federated tool", func(t *testing.T) {
		rec := call(t, `{"method":"tools/call","params":{"name":"ns1__action","arguments":{}},"via":"native_mount"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (source mismatch): %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("via is required", func(t *testing.T) {
		rec := call(t, `{"method":"tools/call","params":{"name":"action","arguments":{}}}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("only tools/call and resources/read are relayed", func(t *testing.T) {
		rec := call(t, `{"method":"prompts/get","params":{"name":"p"},"via":"ns1__mount"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestHandleAppProxy_ResourcesRead is the resource-side twin of the
// tool-call test: reads relayed over the app-proxy stay scoped to the
// mounting tool's own MCP server, exactly like the Via branch of
// handleReadResource.
func TestHandleAppProxy_ResourcesRead(t *testing.T) {
	readFrom, readURI := "", ""
	host := &fakeScopedHost{
		fakeHost: &fakeHost{
			tools: []Tool{{Name: "ns1__mount", Visibility: []string{"app"}}},
		},
		toolSource: func(ctx context.Context, name string) (string, bool) {
			if name == "ns1__mount" {
				return "server-a", true
			}
			return "", false
		},
		readResourceFromSource: func(ctx context.Context, source, uri string) (ResourceResult, error) {
			readFrom, readURI = source, uri
			return ResourceResult{URI: uri, Text: "<html></html>", MimeType: "text/html;profile=mcp-app"}, nil
		},
	}
	s := newTestServer(t, host)

	call := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/chat/api/app-proxy", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleAppProxy(rec, req)
		return rec
	}

	t.Run("a read is scoped to the mounting tool's server", func(t *testing.T) {
		rec := call(t, `{"method":"resources/read","params":{"uri":"ui://x/y.html"},"via":"ns1__mount"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if readFrom != "server-a" || readURI != "ui://x/y.html" {
			t.Errorf("read from %q uri %q, want server-a / ui://x/y.html", readFrom, readURI)
		}
	})

	t.Run("an unresolvable via denies rather than allows", func(t *testing.T) {
		rec := call(t, `{"method":"resources/read","params":{"uri":"ui://x/y.html"},"via":"ns2__mount"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a missing uri is a bad request", func(t *testing.T) {
		rec := call(t, `{"method":"resources/read","params":{},"via":"ns1__mount"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})
}

// Persona params reach the model even when the browser doesn't send them
// (it only round-trips the params it has fields for); request params win.
func TestHandleChatMergesPersonaParams(t *testing.T) {
	var got CompleteRequest
	host := &fakeHost{
		complete: func(ctx context.Context, req CompleteRequest, events chan<- Event) error {
			got = req
			events <- Event{Type: EventDone, FinishReason: FinishStop}
			return nil
		},
	}
	s, err := New(Config{Prefix: "/chat", Host: host, PersonaSource: StaticPersonas{
		{ID: "p", Name: "P", Params: map[string]interface{}{"reasoning_effort": "low", "temperature": 0.2}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model":"m","persona_id":"p","messages":[{"role":"user","content":"hi"}],"params":{"temperature":0.9}}`
	rec := httptest.NewRecorder()
	s.handleChat(rec, httptest.NewRequest(http.MethodPost, "/chat/api/chat", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d (%s)", rec.Code, rec.Body.String())
	}
	if got.Params["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v, want the persona's low", got.Params["reasoning_effort"])
	}
	if got.Params["temperature"] != 0.9 {
		t.Errorf("temperature = %v, want the request's 0.9", got.Params["temperature"])
	}
	if body := OpenAIChatRequest(got); body["reasoning_effort"] != "low" {
		t.Errorf("upstream body reasoning_effort = %v", body["reasoning_effort"])
	}
}
