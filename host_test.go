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
