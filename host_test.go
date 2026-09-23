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
