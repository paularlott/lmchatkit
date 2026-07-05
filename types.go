// Package webchat is a self-contained chat UI + backend protocol that mounts
// into any HTTP server. The host implements [Host] to provide an LLM
// completion stream and (optionally) MCP tools, prompts and resources; webchat
// owns the frontend bundle, the chat session protocol, persona loading and
// slash-command loading.
//
// Routes are mounted under a configurable prefix (typically /chat) via
// [Server.Mount]. Auth is the host's responsibility — pass an [AuthMiddleware]
// in [Config] and it wraps every webchat handler.
package webchat

import (
	"context"
	"encoding/json"
)

// Role identifies the speaker of a chat message. Mirrors OpenAI's role names
// so hosts that proxy to OpenAI-compatible APIs can pass messages through
// verbatim.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool       Role = "tool"
)

// Message is one turn in a conversation. Content is normally a string, but
// when a message carries resource attachments the frontend may send it as
// an OpenAI-compatible content array: [{type:"text",text:"..."},{type:"image_url",...}].
// The Go type uses interface{} to accept both shapes and pass them through
// to the host's Complete implementation verbatim.
type Message struct {
	Role       Role       `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
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
	ID      string `json:"id"`
	Label   string `json:"label,omitempty"`   // human-friendly label; falls back to ID
	Provider string `json:"provider,omitempty"` // optional source tag for the UI
}

// Tool describes one MCP tool exposed to the chat. InputSchema is the JSON
// schema for arguments (as exposed by MCP tools/list); the frontend uses it
// to render argument hints when confirming a tool call.
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
}

// ToolResult is the outcome of a tool call. Content is the model-facing text
// (typically the MCP tool response). isError flags the result as an error so
// the model knows not to treat Content as a successful payload.
type ToolResult struct {
	Content  string `json:"content"`
	IsError  bool   `json:"is_error,omitempty"`
}

// PromptArgument is one named argument a prompt accepts.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Prompt is one MCP prompt exposed to the chat.
type Prompt struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Arguments   []PromptArgument  `json:"arguments,omitempty"`
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
type ResourceResult struct {
	URI      string `json:"uri"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
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
	EventDelta      EventType = "delta"       // partial assistant text
	EventReasoning  EventType = "reasoning"   // partial reasoning/thinking text (separate from visible content)
	EventToolCall   EventType = "tool_call"   // model requested a tool call; frontend must confirm + execute then resubmit
	EventDone       EventType = "done"        // stream complete; carry usage/finish_reason
	EventError      EventType = "error"       // stream failed
)

// FinishReason explains why the stream ended.
type FinishReason string

const (
	FinishStop        FinishReason = "stop"
	FinishToolCalls   FinishReason = "tool_calls"
	FinishLength      FinishReason = "length"
)

// Event is one streamed server-sent event in the chat protocol. Type
// determines which fields are meaningful.
type Event struct {
	Type         EventType     `json:"type"`
	Delta        string        `json:"delta,omitempty"`
	Reasoning    string        `json:"reasoning,omitempty"` // carries EventReasoning fragments
	ToolCall     *ToolCall     `json:"tool_call,omitempty"`
	FinishReason FinishReason  `json:"finish_reason,omitempty"`
	Usage        *Usage        `json:"usage,omitempty"`
	Error        string        `json:"error,omitempty"`
}

// Usage reports token counts for a completion, if known.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

// Host is the contract between webchat and its embedding application. Every
// method takes a context so hosts can enforce timeouts / cancellation. Any
// method may return an error; webchat surfaces it to the user.
//
// All methods must be safe for concurrent use: webchat is stateless and a
// single Server may serve many simultaneous chat sessions across many users.
type Host interface {
	// Models returns the models the chat user may select from. The list may
	// be empty if the host has no concept of model picker (rare).
	Models(ctx context.Context) ([]Model, error)

	// Complete streams a chat completion for the given request. Implementations
	// push events onto events (never block on a full channel — webchat's
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

// PersonaSource is the backend behind /api/personas. The default
// implementation reads TOML files from a watched directory; hosts with a
// database (or a single system-defined persona) supply their own.
//
// Personas is called on every /api/personas request so a DB-backed source
// always reflects current state without needing a watcher.
type PersonaSource interface {
	Personas(ctx context.Context) ([]Persona, error)
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
