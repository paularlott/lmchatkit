// Package lmchatkit is a self-contained chat UI + backend protocol that mounts
// into any HTTP server. The host implements [Host] to provide an LLM
// completion stream and (optionally) MCP tools, prompts and resources; lmchatkit
// owns the frontend bundle, the chat session protocol, persona loading and
// slash-command loading.
//
// Routes are mounted under a configurable prefix (typically /chat) via
// [Server.Mount]. Auth is the host's responsibility — pass an [AuthMiddleware]
// in [Config] and it wraps every lmchatkit handler.
package lmchatkit

import (
	"context"
	"encoding/json"

	mcplib "github.com/paularlott/mcp"
)

// Role identifies the speaker of a chat message. Mirrors OpenAI's role names
// so hosts that proxy to OpenAI-compatible APIs can pass messages through
// verbatim.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in a conversation. Content is normally a string, but
// when a message carries resource attachments the frontend may send it as
// an OpenAI-compatible content array. The Go type uses interface{} to
// accept both shapes and pass them through to the host's Complete
// implementation verbatim.
//
// ID, Thinking, and Info are UI-only fields that the frontend sets on
// messages for display purposes (Alpine x-for keys, reasoning disclosure,
// info cards). They are persisted in conversation history so the UI can
// reconstruct the exact display on reload, but the chat handler strips
// them before passing messages to Host.Complete — the LLM never sees them.
//
// Content does NOT use omitempty — empty string content must be preserved
// when saving/loading conversations from the history store. With omitempty,
// an empty assistant message would lose its "content" key entirely, and
// on reload the browser would see `undefined` instead of `""`.
//
// IsError, StructuredContent, and UI are meaningful only on a RoleTool
// message (the message recording one tool call's result). They mirror the
// same-named fields on ToolResult, persisted here so the frontend can
// reconstruct a tool call's full display — including re-mounting an MCP
// Apps view — from a reloaded conversation, without re-invoking the tool
// itself (which may not be idempotent; re-running it could show a
// different result than what actually happened, or double a side effect).
type Message struct {
	ID                string                 `json:"id,omitempty"`
	Role              Role                   `json:"role"`
	Content           any                    `json:"content"`
	Thinking          string                 `json:"thinking,omitempty"`
	Info              map[string]interface{} `json:"info,omitempty"`
	ToolCalls         []ToolCall             `json:"tool_calls,omitempty"`
	ToolCallID        string                 `json:"tool_call_id,omitempty"`
	ToolName          string                 `json:"tool_name,omitempty"`
	IsError           bool                   `json:"is_error,omitempty"`
	StructuredContent any                    `json:"structured_content,omitempty"`
	UI                *mcplib.UIToolMeta     `json:"ui,omitempty"`
}

// ToolCall is a single tool invocation requested by the model. Arguments is
// the raw JSON arguments string (the host parses it according to the tool's
// input schema).
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Model describes one model the chat user can pick from.
type Model struct {
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`    // human-friendly label; falls back to ID
	Provider string `json:"provider,omitempty"` // optional source tag for the UI
}

// Tool describes one MCP tool exposed to the chat. InputSchema is the JSON
// schema for arguments (as exposed by MCP tools/list); the frontend uses it
// to render argument hints when confirming a tool call.
//
// Visibility is the tool's _meta.ui.visibility (per the MCP Apps extension,
// SEP-1865): who may call it — "model" (the agent), "app" (a mounted view),
// or both. Nil/empty means both, per the spec's default. ListTools returns
// every tool regardless of visibility (callers needing the model-facing
// subset use [FilterToolsForModel]); it's carried here so the /api/tools/call
// handler can enforce it without a second round trip to the MCP server.
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
	Visibility  []string               `json:"-"`
}

// visibilityAllows reports whether visibility (a Tool's _meta.ui.visibility,
// or nil) permits the given role ("model" or "app"). Per the MCP Apps spec,
// an empty/nil visibility defaults to both.
func visibilityAllows(visibility []string, role string) bool {
	if len(visibility) == 0 {
		return true
	}
	for _, v := range visibility {
		if v == role {
			return true
		}
	}
	return false
}

// FilterToolsForModel returns the subset of tools visible to the model,
// per the MCP Apps extension's tools/list MUST: a tool whose
// _meta.ui.visibility doesn't include "model" (e.g. an app-only action tool
// like a form submission) must never reach the agent's own tool list, even
// though it's still a real, callable tool for a mounted view. Hosts building
// the LLM-facing tool array from [Host.ListTools]'s result MUST filter
// through this first — StandardHost's ListTools deliberately returns every
// tool unfiltered, since the same list also backs the /api/tools/call
// visibility check for app-initiated calls, which needs the app-only tools.
func FilterToolsForModel(tools []Tool) []Tool {
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if visibilityAllows(t.Visibility, "model") {
			out = append(out, t)
		}
	}
	return out
}

// ToolResult is the outcome of a tool call. Content is the model-facing text
// (typically the MCP tool response). isError flags the result as an error so
// the model knows not to treat Content as a successful payload.
//
// StructuredContent and UI are frontend-only extras, not sent to the model:
// StructuredContent is the tool response's raw structuredContent (if any),
// which Content's text-only flattening otherwise discards entirely.
// UI is the called tool's own _meta.ui (per the MCP Apps extension,
// SEP-1865), when the underlying MCP server declared one — the frontend uses
// it to decide whether to render an interactive view for this result.
type ToolResult struct {
	Content           string             `json:"content"`
	IsError           bool               `json:"is_error,omitempty"`
	StructuredContent any                `json:"structured_content,omitempty"`
	UI                *mcplib.UIToolMeta `json:"ui,omitempty"`
}

// PromptArgument is one named argument a prompt accepts.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Prompt is one MCP prompt exposed to the chat.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptMessage is one message produced by rendering a prompt.
type PromptMessage struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// PromptResult is the rendered output of GetPrompt.
type PromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// Resource is one MCP resource (static or templated) exposed to the chat.
// When Template is true, URI contains {var} placeholders the user must fill.
type Resource struct {
	URI         string `json:"uri"`
	Template    bool   `json:"template,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mime_type,omitempty"`
}

// ResourceResult is the content of a read resource. Text is used for textual
// content; Blob carries base64-encoded binary content.
//
// UI carries the resource's own _meta.ui (per the MCP Apps extension,
// SEP-1865) — CSP/permissions/domain/prefersBorder hints a compliant host
// applies before rendering the content in a sandboxed iframe. Note its JSON
// field names (e.g. "resourceUri", "connectDomains") are camelCase, unlike
// the rest of this package's snake_case convention — this reuses the mcp
// library's own wire type directly rather than duplicating it.
type ResourceResult struct {
	URI      string                 `json:"uri"`
	Text     string                 `json:"text,omitempty"`
	Blob     string                 `json:"blob,omitempty"`
	MimeType string                 `json:"mime_type,omitempty"`
	UI       *mcplib.UIResourceMeta `json:"ui,omitempty"`
}

// CompleteRequest is the host-facing request to stream a chat completion.
// Messages is the full conversation including any prior tool results.
// Tools is the subset of tools the user has enabled for this chat (may be
// empty). Params is model parameters merged from persona + per-request
// overrides (temperature, max_tokens, etc.); the host passes it through to
// the underlying LLM API as it sees fit.
type CompleteRequest struct {
	Model    string                 `json:"model"`
	Messages []Message              `json:"messages"`
	Tools    []Tool                 `json:"tools,omitempty"`
	Params   map[string]interface{} `json:"params,omitempty"`
}

// EventType identifies one SSE event in the chat stream protocol.
type EventType string

const (
	EventDelta     EventType = "delta"     // partial assistant text
	EventReasoning EventType = "reasoning" // partial reasoning/thinking text (separate from visible content)
	EventToolCall  EventType = "tool_call" // model requested a tool call; frontend must confirm + execute then resubmit
	EventDone      EventType = "done"      // stream complete; carry usage/finish_reason
	EventError     EventType = "error"     // stream failed
)

// FinishReason explains why the stream ended.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"
)

// Event is one streamed server-sent event in the chat protocol. Type
// determines which fields are meaningful.
type Event struct {
	Type         EventType    `json:"type"`
	Delta        string       `json:"delta,omitempty"`
	Reasoning    string       `json:"reasoning,omitempty"` // carries EventReasoning fragments
	ToolCall     *ToolCall    `json:"tool_call,omitempty"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
	Usage        *Usage       `json:"usage,omitempty"`
	Error        string       `json:"error,omitempty"`
}

// Usage reports token counts for a completion, if known.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

// Host is the contract between lmchatkit and its embedding application. Every
// method takes a context so hosts can enforce timeouts / cancellation. Any
// method may return an error; lmchatkit surfaces it to the user.
//
// All methods must be safe for concurrent use: lmchatkit is stateless and a
// single Server may serve many simultaneous chat sessions across many users.
type Host interface {
	// Models returns the models the chat user may select from. The list may
	// be empty if the host has no concept of model picker (rare).
	Models(ctx context.Context) ([]Model, error)

	// Complete streams a chat completion for the given request. Implementations
	// push events onto events (never block on a full channel — lmchatkit's
	// channel is buffered) and return when the stream is finished. If the
	// model emitted tool calls, emit one [EventToolCall] per call and return
	// with FinishReason == FinishToolCalls — the frontend will execute the
	// tools via CallTool and resubmit the conversation.
	Complete(ctx context.Context, req CompleteRequest, events chan<- Event) error

	// ListTools returns the MCP tools available to chat. May return nil/empty
	// if no tools are configured.
	ListTools(ctx context.Context) ([]Tool, error)

	// CallTool invokes a tool by name with raw-JSON arguments. The arguments
	// are exactly what the model produced (after the user confirmed), so the
	// host is responsible for any validation.
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, error)

	// ListPrompts / GetPrompt expose MCP prompts. GetPrompt renders the prompt
	// with the given arguments.
	ListPrompts(ctx context.Context) ([]Prompt, error)
	GetPrompt(ctx context.Context, name string, args map[string]string) (PromptResult, error)

	// ListResources / ReadResource expose MCP resources. ReadResource takes a
	// concrete URI (the caller is responsible for expanding templates).
	ListResources(ctx context.Context) ([]Resource, error)
	ReadResource(ctx context.Context, uri string) (ResourceResult, error)
}

// AllToolsHost is an optional interface a Host can implement to list tools
// that ListTools omits because they're discoverable-only (e.g. an MCP
// server's search-only tools, reached by the model through a meta-tool like
// execute_tool rather than listed directly, to save context). An app view's
// own tool calls (POST /api/tools/call, Source: "app") name such a tool
// directly — chat.js resolves it to its real, namespaced name itself — so
// the visibility check in toolVisibilityAllows needs to find it, even though
// it would never appear in the model-facing list. A Host whose tools are
// never discoverable-only need not implement this; lmchatkit falls back to
// ListTools, same as before this existed.
type AllToolsHost interface {
	Host
	ListAllTools(ctx context.Context) ([]Tool, error)
}

// PersonaSource is the backend behind /api/personas. The default
// implementation reads TOML files from a watched directory; hosts with a
// database (or a single system-defined persona) supply their own.
//
// Personas is called on every /api/personas request so a DB-backed source
// always reflects current state without needing a watcher.
type PersonaSource interface {
	Personas(ctx context.Context) ([]Persona, error)
}

// SourceScopedHost is an optional interface a Host can implement to expose
// which underlying MCP server each tool/resource belongs to. lmchatkit's
// /api/tools/call and /api/resources/read handlers use it, when present, to
// enforce that a mounted MCP Apps view may only reach a tool or resource
// belonging to the same server as the tool that mounted it — visibility
// (_meta.ui.visibility) alone can't provide this: two different federated
// servers connected without namespace prefixes can expose same-shaped
// "app"-visible tools/resources under names that collide or simply aren't
// distinguishable by name alone.
//
// A Host that aggregates only a single, non-federated MCP server (no
// cross-server ambiguity is possible) need not implement this — lmchatkit
// falls back to allowing the call/read unchecked (beyond the existing
// visibility check) when the Host doesn't implement it.
type SourceScopedHost interface {
	// ToolSource returns an opaque, stable identifier for the MCP server
	// the named tool belongs to ("" for a tool the host serves natively,
	// as opposed to a federated remote), and whether name is a recognized
	// tool at all. Implementations must fail closed: an error resolving
	// the tool must return ok == false, never a guessed source.
	ToolSource(ctx context.Context, name string) (source string, ok bool)

	// ReadResourceFromSource reads uri restricted to the server identified
	// by source (as returned by ToolSource): "" reads only natively-served
	// resources, a non-empty source reads only from that one remote server
	// — never falling through to any other server the way an unscoped
	// ReadResource might.
	ReadResourceFromSource(ctx context.Context, source, uri string) (ResourceResult, error)
}

// SystemPromptAugmenter is an optional interface a Host can implement to
// dynamically augment the system prompt on every chat request. The chat
// handler calls AugmentSystemPrompt with the current system prompt content
// (from the persona) and uses the returned string when forwarding to the
// LLM. The stored conversation is not modified — the augmentation is
// transient, recomputed on each request so it always reflects current state
// (e.g. skills added/removed at runtime).
type SystemPromptAugmenter interface {
	AugmentSystemPrompt(ctx context.Context, current string) string
}

// CommandSource is the backend behind /api/commands. Same contract as
// [PersonaSource]: a file-watching default exists, hosts with a database
// (e.g. per-user commands in knot) implement their own.
type CommandSource interface {
	Commands(ctx context.Context) ([]SlashCommand, error)
}

// StaticPersonas is a PersonaSource backed by a fixed slice. Useful for
// single-tenant hosts that have one system-defined persona (e.g. knot).
type StaticPersonas []Persona

func (s StaticPersonas) Personas(ctx context.Context) ([]Persona, error) { return s, nil }

// StaticCommands is a CommandSource backed by a fixed slice.
type StaticCommands []SlashCommand

func (s StaticCommands) Commands(ctx context.Context) ([]SlashCommand, error) { return s, nil }
