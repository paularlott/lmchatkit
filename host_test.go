package lmchatkit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mcplib "github.com/paularlott/mcp"
)

const testUIResourceURI = "ui://dashboard/dashboard.html"

// buildTestMCPServer returns a server with:
//   - "sales_report": a UI-linked tool (resourceUri + icon) returning structured content
//   - "ping": a plain tool with no UI metadata
//   - the paired ui:// resource with CSP hints
func buildTestMCPServer() *mcplib.Server {
	srv := mcplib.NewServer("test", "0.0.1")

	srv.RegisterTool(
		mcplib.NewTool("sales_report", "Get the sales report").
			UIResource(testUIResourceURI, "model", "app").
			Icons(mcplib.Icon{Src: "https://example.com/icon.png", MimeType: "image/png"}),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			return mcplib.NewToolResponseStructured(map[string]any{"records": []string{"a", "b"}}), nil
		},
	)

	srv.RegisterTool(
		mcplib.NewTool("ping", "Plain tool with no UI metadata"),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			return mcplib.NewToolResponseText("pong"), nil
		},
	)

	srv.RegisterTool(
		mcplib.NewTool("add_sale", "App-only action tool, called by the dashboard's own form").
			Visibility("app"),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			return mcplib.NewToolResponseText("added"), nil
		},
	)

	srv.RegisterResource(
		mcplib.NewResource(testUIResourceURI, "Sales Dashboard", "the dashboard ui", mcplib.UIAppMimeType).
			UIMeta(mcplib.UIResourceMeta{CSP: &mcplib.UICSP{ResourceDomains: []string{"https://cdn.example.com"}}}),
		func(ctx context.Context, req *mcplib.ResourceRequest) (*mcplib.ResourceResponse, error) {
			return mcplib.NewUIResourceResponseText(testUIResourceURI, "<html></html>", &mcplib.UIResourceMeta{
				CSP: &mcplib.UICSP{ResourceDomains: []string{"https://cdn.example.com"}},
			}), nil
		},
	)

	return srv
}

func TestStandardHostCallTool_NativeUITool(t *testing.T) {
	srv := buildTestMCPServer()
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	res, err := h.CallTool(context.Background(), "sales_report", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.UI == nil {
		t.Fatal("expected ToolResult.UI to be populated for a native UI-linked tool")
	}
	if res.UI.ResourceURI != testUIResourceURI {
		t.Errorf("ResourceURI = %q, want %q", res.UI.ResourceURI, testUIResourceURI)
	}
	if len(res.UI.Visibility) != 2 {
		t.Errorf("Visibility = %v, want [model app]", res.UI.Visibility)
	}
	if res.StructuredContent == nil {
		t.Error("expected StructuredContent to be populated, got nil")
	}
}

func TestStandardHostCallTool_PlainToolHasNoUI(t *testing.T) {
	srv := buildTestMCPServer()
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	res, err := h.CallTool(context.Background(), "ping", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.UI != nil {
		t.Errorf("expected UI to stay nil for a plain tool, got %+v", res.UI)
	}
	if res.Content != "pong" {
		t.Errorf("Content = %q, want %q", res.Content, "pong")
	}
}

func TestStandardHostReadResource_UIResource(t *testing.T) {
	srv := buildTestMCPServer()
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	res, err := h.ReadResource(context.Background(), testUIResourceURI)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if res.UI == nil {
		t.Fatal("expected ResourceResult.UI to be populated")
	}
	if res.UI.CSP == nil || len(res.UI.CSP.ResourceDomains) != 1 || res.UI.CSP.ResourceDomains[0] != "https://cdn.example.com" {
		t.Errorf("CSP = %+v, want ResourceDomains [https://cdn.example.com]", res.UI.CSP)
	}
}

// TestStandardHostCallTool_FederatedUITool proves the JSON round-trip
// extraction handles a federated tool's metadata, which — unlike a native
// tool's — always arrives as map[string]any (never the concrete
// mcplib.UIToolMeta struct) because it was deserialized from the remote
// server's HTTP response into an `any`-typed Meta field. A naive type
// assertion (meta["ui"].(mcplib.UIToolMeta)) would silently return nil here
// even though the data is present.
func TestStandardHostCallTool_FederatedUITool(t *testing.T) {
	remote := buildTestMCPServer()
	ts := httptest.NewServer(http.HandlerFunc(remote.HandleRequest))
	defer ts.Close()

	client := mcplib.NewClient(ts.URL, nil, "")
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("client.Initialize: %v", err)
	}

	aggregator := mcplib.NewServer("aggregator", "0.0.1")
	if err := aggregator.ReplaceRemoteServers([]mcplib.RemoteServerEntry{
		{Client: client, Visibility: mcplib.ToolVisibilityNative},
	}); err != nil {
		t.Fatalf("ReplaceRemoteServers: %v", err)
	}

	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return aggregator }}

	res, err := h.CallTool(context.Background(), "sales_report", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.UI == nil {
		t.Fatal("expected ToolResult.UI to be populated for a federated UI-linked tool")
	}
	if res.UI.ResourceURI != testUIResourceURI {
		t.Errorf("ResourceURI = %q, want %q", res.UI.ResourceURI, testUIResourceURI)
	}
}

// TestStandardHostCallTool_DiscoverableUIToolViaExecuteTool reproduces a real
// bug: a discoverable (search-only) tool is never called by its own name —
// the model calls execute_tool with {"name": ..., "parameters": ...} — so a
// naive ToolResult.UI lookup keyed on the outer call name ("execute_tool")
// never finds the tool's _meta.ui, and a discoverable MCP Apps tool would
// silently never render.
func TestStandardHostCallTool_DiscoverableUIToolViaExecuteTool(t *testing.T) {
	srv := mcplib.NewServer("test", "0.0.1")
	srv.RegisterTool(
		mcplib.NewTool("spin_wheel", "Spin the prize wheel").
			UIResource(testUIResourceURI, "model", "app").
			Discoverable(),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			return mcplib.NewToolResponseText("Sticker Pack"), nil
		},
	)
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	args, _ := json.Marshal(map[string]any{"name": "spin_wheel", "parameters": map[string]any{}})
	res, err := h.CallTool(context.Background(), mcplib.ExecuteToolName, args)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Content != "Sticker Pack" {
		t.Errorf("Content = %q, want %q (execute_tool must still dispatch to the real tool)", res.Content, "Sticker Pack")
	}
	if res.UI == nil {
		t.Fatal("expected ToolResult.UI to be populated for a discoverable UI-linked tool called via execute_tool")
	}
	if res.UI.ResourceURI != testUIResourceURI {
		t.Errorf("ResourceURI = %q, want %q", res.UI.ResourceURI, testUIResourceURI)
	}
}

// TestStandardHostListAllTools_IncludesDiscoverable pins down the difference
// between ListTools and ListAllTools: a discoverable (search-only) tool is
// excluded from the former (the model-facing list, by design — that's what
// "discoverable" means) but must be present in the latter, which
// toolVisibilityAllows needs to find a tool an app view calls directly by
// its real name.
func TestStandardHostListAllTools_IncludesDiscoverable(t *testing.T) {
	srv := mcplib.NewServer("test", "0.0.1")
	srv.RegisterTool(
		mcplib.NewTool("spin_wheel", "Spin the prize wheel").
			UIResource(testUIResourceURI, "model", "app").
			Discoverable(),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			return mcplib.NewToolResponseText("ok"), nil
		},
	)
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	listed, err := h.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range listed {
		if tool.Name == "spin_wheel" {
			t.Fatalf("ListTools unexpectedly included discoverable tool %q", tool.Name)
		}
	}

	all, err := h.ListAllTools(context.Background())
	if err != nil {
		t.Fatalf("ListAllTools: %v", err)
	}
	var found bool
	for _, tool := range all {
		if tool.Name == "spin_wheel" {
			found = true
		}
	}
	if !found {
		t.Fatal("ListAllTools must include the discoverable tool")
	}
}

// TestStandardHostCallTool_ExecuteToolLegacyArgumentsKey covers the
// "arguments" key handleExecuteTool falls back to when "parameters" is
// absent — resolvedToolName only needs the "name" key, but this pins down
// that the fallback key doesn't somehow break name resolution.
func TestStandardHostCallTool_ExecuteToolLegacyArgumentsKey(t *testing.T) {
	srv := mcplib.NewServer("test", "0.0.1")
	srv.RegisterTool(
		mcplib.NewTool("spin_wheel", "Spin the prize wheel").
			UIResource(testUIResourceURI).
			Discoverable(),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			return mcplib.NewToolResponseText("ok"), nil
		},
	)
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	args, _ := json.Marshal(map[string]any{"name": "spin_wheel", "arguments": map[string]any{}})
	res, err := h.CallTool(context.Background(), mcplib.ExecuteToolName, args)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.UI == nil || res.UI.ResourceURI != testUIResourceURI {
		t.Errorf("expected UI.ResourceURI = %q, got %+v", testUIResourceURI, res.UI)
	}
}

// TestStandardHostCallTool_ExecuteToolMissingNameNoUI covers a malformed
// execute_tool call (missing "name") — must not panic and must not report a
// bogus UI link.
func TestStandardHostCallTool_ExecuteToolMissingNameNoUI(t *testing.T) {
	srv := buildTestMCPServer()
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	args, _ := json.Marshal(map[string]any{})
	res, err := h.CallTool(context.Background(), mcplib.ExecuteToolName, args)
	// handleExecuteTool returns a text response ("Tool name is required"),
	// not an error — CallTool must reflect that, not panic or fabricate a UI.
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.UI != nil {
		t.Errorf("expected no UI for a malformed execute_tool call, got %+v", res.UI)
	}
}

// TestStandardHostListTools_PopulatesVisibility and
// TestFilterToolsForModel_DropsAppOnlyTools together pin down the MCP Apps
// visibility enforcement path: ListTools must expose each tool's
// _meta.ui.visibility (needed later by handleCallTool's authorization
// check against the *unfiltered* list), and FilterToolsForModel — what
// chat.go actually sends to the LLM — must drop a tool whose visibility
// excludes "model", per the extension's tools/list MUST.
func TestStandardHostListTools_PopulatesVisibility(t *testing.T) {
	srv := buildTestMCPServer()
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	tools, err := h.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	byName := map[string]Tool{}
	for _, t := range tools {
		byName[t.Name] = t
	}

	if got := byName["add_sale"].Visibility; len(got) != 1 || got[0] != "app" {
		t.Errorf("add_sale visibility = %v, want [app]", got)
	}
	if got := byName["sales_report"].Visibility; len(got) != 2 {
		t.Errorf("sales_report visibility = %v, want [model app]", got)
	}
	if got := byName["ping"].Visibility; got != nil {
		t.Errorf("ping visibility = %v, want nil (no ui meta at all)", got)
	}
	// ListTools itself must NOT filter — handleCallTool's authorization
	// check for an app-initiated call needs add_sale to still be here.
	if _, ok := byName["add_sale"]; !ok {
		t.Fatal("add_sale missing from ListTools' result — it must return every tool, unfiltered")
	}
}

func TestFilterToolsForModel_DropsAppOnlyTools(t *testing.T) {
	srv := buildTestMCPServer()
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	tools, err := h.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	filtered := FilterToolsForModel(tools)

	names := map[string]bool{}
	for _, t := range filtered {
		names[t.Name] = true
	}
	if names["add_sale"] {
		t.Error("add_sale (visibility=[app]) must be excluded from the model-facing list")
	}
	if !names["ping"] || !names["sales_report"] {
		t.Errorf("expected ping and sales_report in the model-facing list, got %v", names)
	}
}

// fakeSourcedProvider is a minimal SourcedToolProvider double: one tool
// namespaced "remote1__", "owned" by a single opaque source identifier, with
// a single resource only that source can read.
type fakeSourcedProvider struct {
	source      string
	toolName    string
	resourceURI string
	resource    *mcplib.ResourceResponse
}

func (p *fakeSourcedProvider) GetTools(ctx context.Context) ([]mcplib.MCPTool, error) {
	return []mcplib.MCPTool{{Name: p.toolName}}, nil
}

func (p *fakeSourcedProvider) ExecuteTool(ctx context.Context, name string, args map[string]any) (*mcplib.ToolResponse, error) {
	return nil, mcplib.ErrUnknownTool
}

func (p *fakeSourcedProvider) ToolSource(ctx context.Context, name string) (string, bool) {
	if name == p.toolName {
		return p.source, true
	}
	return "", false
}

func (p *fakeSourcedProvider) ReadResourceFromSource(ctx context.Context, source, uri string) (*mcplib.ResourceResponse, error) {
	if source != p.source || uri != p.resourceURI {
		return nil, mcplib.ErrUnknownResource
	}
	return p.resource, nil
}

var _ SourcedToolProvider = (*fakeSourcedProvider)(nil)

// fakePlainProvider implements only mcplib.ToolProvider — no source
// awareness — standing in for e.g. a script- or method-tools provider
// alongside a SourcedToolProvider in the same request, the way a real host
// attaches several distinct providers together.
type fakePlainProvider struct{ toolName string }

func (p *fakePlainProvider) GetTools(ctx context.Context) ([]mcplib.MCPTool, error) {
	return []mcplib.MCPTool{{Name: p.toolName}}, nil
}

func (p *fakePlainProvider) ExecuteTool(ctx context.Context, name string, args map[string]any) (*mcplib.ToolResponse, error) {
	return nil, mcplib.ErrUnknownTool
}

var _ mcplib.ToolProvider = (*fakePlainProvider)(nil)

// TestStandardHostToolSource_MultipleProvidersAttachedSeparately is the
// regression test for the real bug this all exists to catch: a host must
// attach each request-scoped provider to context individually
// (mcplib.WithToolProviders(ctx, p1, p2, ...)), never pre-merged via
// mcplib.NewMultiProvider into one. MultiProvider only forwards
// GetTools/ExecuteTool — wrapping a SourcedToolProvider inside one makes it
// invisible to the type assertion in ToolSource/ReadResourceFromSource below,
// so ToolSource always fails and MCP Apps silently never renders for that
// provider's tools, exactly as if this file's fix didn't exist. This test
// attaches a plain provider and a sourced provider side by side (unmerged),
// which is the only way to keep the sourced one type-assertable.
func TestStandardHostToolSource_MultipleProvidersAttachedSeparately(t *testing.T) {
	srv := mcplib.NewServer("test", "0.0.1")
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	plain := &fakePlainProvider{toolName: "script_tool"}
	sourced := &fakeSourcedProvider{
		source:      "user-server-42",
		toolName:    "remote1__spin_wheel",
		resourceURI: testUIResourceURI,
		resource:    &mcplib.ResourceResponse{Contents: []mcplib.ResourceContent{{URI: testUIResourceURI, Text: "<html></html>"}}},
	}
	ctx := mcplib.WithToolProviders(context.Background(), plain, sourced)

	source, ok := h.ToolSource(ctx, "remote1__spin_wheel")
	if !ok {
		t.Fatal("expected ToolSource to find the sourced provider among several attached providers")
	}
	if source != "user-server-42" {
		t.Errorf("source = %q, want %q", source, "user-server-42")
	}

	// The plain provider doesn't implement SourcedToolProvider at all — its
	// tool must fail closed (no source), not panic or false-positive.
	if _, ok := h.ToolSource(ctx, "script_tool"); ok {
		t.Error("expected ToolSource to report no source for a plain provider's tool")
	}
}

// TestStandardHostToolSource_ResolvesContextAttachedProvider reproduces a
// real bug: a Host backed by per-request providers (mcplib.WithToolProviders)
// rather than servers registered directly on the *mcp.Server — e.g. a
// per-user set of remote MCP servers — had no way to satisfy
// SourceScopedHost for those tools at all. srv.ToolSource only ever sees
// natively-registered tools and servers registered via RegisterRemoteServer*,
// so /api/resources/read always 403'd for a context-attached provider's
// tools, and chat.js swallows that as "no app renders" rather than an error.
func TestStandardHostToolSource_ResolvesContextAttachedProvider(t *testing.T) {
	srv := mcplib.NewServer("test", "0.0.1")
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	provider := &fakeSourcedProvider{
		source:      "user-server-42",
		toolName:    "remote1__spin_wheel",
		resourceURI: testUIResourceURI,
		resource:    &mcplib.ResourceResponse{Contents: []mcplib.ResourceContent{{URI: testUIResourceURI, Text: "<html></html>"}}},
	}
	ctx := mcplib.WithToolProviders(context.Background(), provider)

	source, ok := h.ToolSource(ctx, "remote1__spin_wheel")
	if !ok {
		t.Fatal("expected ToolSource to resolve the tool via the context-attached provider")
	}
	if source != "user-server-42" {
		t.Errorf("source = %q, want %q", source, "user-server-42")
	}

	res, err := h.ReadResourceFromSource(ctx, source, testUIResourceURI)
	if err != nil {
		t.Fatalf("ReadResourceFromSource: %v", err)
	}
	if res.Text != "<html></html>" {
		t.Errorf("Text = %q, want the provider's resource content", res.Text)
	}
}

// TestStandardHostToolSource_UnknownToolFailsClosed ensures a name no
// provider or the server recognizes reports ok=false, never a guessed source.
func TestStandardHostToolSource_UnknownToolFailsClosed(t *testing.T) {
	srv := mcplib.NewServer("test", "0.0.1")
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	provider := &fakeSourcedProvider{source: "user-server-42", toolName: "remote1__spin_wheel"}
	ctx := mcplib.WithToolProviders(context.Background(), provider)

	if _, ok := h.ToolSource(ctx, "ghost__tool"); ok {
		t.Error("expected ToolSource to fail closed for an unrecognized tool")
	}
}

// TestStandardHostToolSource_NativeTakesPriority ensures a native tool's
// source ("" per SourceScopedHost's contract) is still reported correctly
// even when a context-attached SourcedToolProvider is also present.
func TestStandardHostToolSource_NativeTakesPriority(t *testing.T) {
	srv := buildTestMCPServer() // registers "ping" natively
	h := &StandardHost{MCPServer: func(ctx context.Context) *mcplib.Server { return srv }}

	provider := &fakeSourcedProvider{source: "user-server-42", toolName: "remote1__spin_wheel"}
	ctx := mcplib.WithToolProviders(context.Background(), provider)

	source, ok := h.ToolSource(ctx, "ping")
	if !ok || source != "" {
		t.Errorf("ToolSource(\"ping\") = (%q, %v), want (\"\", true)", source, ok)
	}
}

// Complete posts to ChatCompletionsURL when set, else OpenAIBaseURL +
// /v1/chat/completions, forwarding the request params.
func TestStandardHostComplete_URLAndParams(t *testing.T) {
	var gotPath string
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	for name, tc := range map[string]struct {
		host     StandardHost
		wantPath string
	}{
		"base url":      {StandardHost{OpenAIBaseURL: srv.URL}, "/v1/chat/completions"},
		"override wins": {StandardHost{OpenAIBaseURL: "http://unused.invalid", ChatCompletionsURL: srv.URL + "/v1beta/openai/chat/completions"}, "/v1beta/openai/chat/completions"},
	} {
		t.Run(name, func(t *testing.T) {
			events := make(chan Event, 16)
			req := CompleteRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}, Params: map[string]interface{}{"reasoning_effort": "low"}}
			if err := tc.host.Complete(context.Background(), req, events); err != nil {
				t.Fatal(err)
			}
			if gotPath != tc.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tc.wantPath)
			}
			if gotBody["reasoning_effort"] != "low" {
				t.Errorf("reasoning_effort not forwarded: %v", gotBody)
			}
		})
	}
}

// Unset params aren't sent: the provider applies its own default.
func TestOpenAIChatRequest_SkipsUnsetParams(t *testing.T) {
	body := OpenAIChatRequest(CompleteRequest{Model: "m", Params: map[string]interface{}{
		"reasoning_effort": "", "temperature": nil, "max_tokens": 100, "top_p": 0.0,
	}})
	for _, k := range []string{"reasoning_effort", "temperature"} {
		if _, sent := body[k]; sent {
			t.Errorf("%s sent: %v", k, body[k])
		}
	}
	if body["max_tokens"] != 100 || body["top_p"] != 0.0 {
		t.Errorf("set params dropped: %v", body)
	}
	if _, sent := OpenAIChatRequest(CompleteRequest{Model: "m"})["reasoning_effort"]; sent {
		t.Error("reasoning_effort sent when never set")
	}
}
