# webchat

A self-contained chat UI + backend protocol that mounts into any Go HTTP server. The host implements the [`Host`](types.go) interface to provide an LLM completion stream and (optionally) MCP tools, prompts, and resources; **webchat** owns the frontend bundle, the streaming chat protocol, persona loading, and slash-command loading.

Built for — and extracted from — `paularlott/llmrouter`, with the contract shaped so other OpenAI-compatible hosts (e.g. `paularlott/knot`) can mount the same UI without forking it.

## Features

- **Streaming chat over a custom SSE protocol** (`delta` / `tool_call` / `done` / `error` events). Not OpenAI-shaped — designed for chat UIs, so tool-call confirmation, per-tool disable, and "always allow this tool in this chat" all live in the frontend without server-side session state.
- **Persona loading** from a watched TOML directory (`system_prompt`, `default_model`, `[params]` table). Hot-reloads on file change. A built-in `Default` persona is always offered even when the dir is empty.
- **Slash commands** from a watched markdown directory. `help.md` → `/help`. `$ARGUMENTS` in the body is spliced with whatever the user typed after the command.
- **MCP pass-through** for tools, prompts, and resources via the `Host` interface — the host decides where they come from.
- **Tool-call confirmation flow** with per-session auto-allow and per-chat disable, persisted in browser `localStorage`. No server-side per-user state.
- **Bundles its own HTML/CSS/JS** — the host just calls `Mount(mux)`. The frontend reuses the host's bundled Alpine + Tailwind; webchat doesn't ship a copy.
- **Auth is host-owned** — pass an `AuthMiddleware` in `Config` and it wraps every webchat handler.

## Install

```
go get github.com/paularlott/webchat
```

Go 1.26+. The package has three direct dependencies: `fsnotify/fsnotify`, `paularlott/cli`, `paularlott/logger`.

## Host contract

Implement [`Host`](types.go):

```go
type Host interface {
    // Models returns the models the chat user may select from.
    Models(ctx context.Context) ([]Model, error)

    // Complete streams a chat completion. Emit EventDelta / EventToolCall
    // values onto events, then return. If you emit tool calls, the frontend
    // will execute them via CallTool and resubmit the conversation.
    Complete(ctx context.Context, req CompleteRequest, events chan<- Event) error

    // MCP pass-through. May return nil/empty if the host has nothing to offer.
    ListTools(ctx context.Context) ([]Tool, error)
    CallTool(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, error)
    ListPrompts(ctx context.Context) ([]Prompt, error)
    GetPrompt(ctx context.Context, name string, args map[string]string) (PromptResult, error)
    ListResources(ctx context.Context) ([]Resource, error)
    ReadResource(ctx context.Context, uri string) (ResourceResult, error)
}
```

`Host` must be safe for concurrent use — webchat is stateless and one `Server` may serve many simultaneous sessions across many users.

## Persona & slash-command sources

Personas and slash commands each come from a pluggable source. Two builtin implementations cover the common cases; hosts with a different backing store (e.g. a database, or a single hard-coded system persona) supply their own.

```go
type PersonaSource interface {
    Personas(ctx context.Context) ([]Persona, error)
}
type CommandSource interface {
    Commands(ctx context.Context) ([]SlashCommand, error)
}
```

The source is consulted on every request, so a DB-backed source always reflects the current row set without a watcher.

| Source                       | When to use                                                                       |
|------------------------------|-----------------------------------------------------------------------------------|
| `Config.PersonasDir` (file)  | Multi-tenant hosts reading TOML from disk (llmrouter's default). Hot-reloads.     |
| `Config.CommandsDir` (file)  | Same, for markdown-defined slash commands.                                        |
| `Config.PersonaSource`       | Overrides `PersonasDir`. Implement yourself for a DB / API / single-system case.  |
| `Config.CommandSource`       | Overrides `CommandsDir`. Per-user commands in knot, for example.                  |
| `StaticPersonas`             | One-liner for "exactly one system-defined persona".                               |
| `StaticCommands`             | Same for commands.                                                                |

For knot's "one persona, one model, no per-user commands" case:

```go
srv, _ := webchat.New(webchat.Config{
    Prefix: "/chat",
    Host:   knotHost,
    PersonaSource: webchat.StaticPersonas{{
        ID: "knot", Name: "Knot", SystemPrompt: knotSystemPrompt, DefaultModel: "knot-1",
    }},
    // CommandsDir / CommandSource left nil — slash commands disabled.
    HostJSFile:  "/assets/knot.js",
    HostCSSFile: "/assets/knot.css",
})
```

When the host returns exactly one persona AND one model, the chat UI skips the persona/model picker entirely and drops the user straight into a conversation. (They can still hit "New Chat" to come back to the picker.)

## Mount

```go
import "github.com/paularlott/webchat"

srv, err := webchat.New(webchat.Config{
    Prefix:       "/chat",
    PersonasDir:  "/etc/myapp/personas",
    CommandsDir:  "/etc/myapp/commands",
    Host:         myHost,
    AuthMiddleware: authMiddleware,  // wraps every handler; nil = no auth
    Title:        "Chat",
    HostJSFile:   "/assets/main.js", // host bundle that boots window.Alpine
    HostCSSFile:  "/assets/main.css", // optional, Tailwind base
    ExtraNav: []webchat.NavItem{
        {Href: "/", Label: "Home"},
    },
})
if err != nil {
    log.Fatal(err)
}
defer srv.Close()
srv.Mount(mux)
```

That registers:

| Route                              | Purpose                                            |
|------------------------------------|----------------------------------------------------|
| `GET /chat`                        | HTML shell (server-rendered; the rest is Alpine)   |
| `GET /chat/api/personas`           | Persona list (incl. built-in `Default`)            |
| `GET /chat/api/commands`           | Slash-command list (incl. rendered markdown body)  |
| `GET /chat/api/models`             | Models from `Host.Models`                          |
| `GET /chat/api/tools`              | Tools from `Host.ListTools`, with `?disabled=`     |
| `POST /chat/api/tools/call`        | Execute one tool                                   |
| `GET /chat/api/prompts`            | Prompts from `Host.ListPrompts`                    |
| `POST /chat/api/prompts/get`       | Render a prompt                                    |
| `GET /chat/api/resources`          | Resources (static + templates) from `Host`         |
| `POST /chat/api/resources/read`    | Read one resource                                  |
| `POST /chat/api/chat`              | Streaming completion (SSE response)                |
| `GET /chat/assets/*`               | Embedded `chat.js` bundle                          |

## Chat protocol

`POST /chat/api/chat` streams Server-Sent Events. Each event is one JSON-encoded [`Event`](types.go) prefixed with `data: ` and terminated by `\n\n`:

```
data: {"type":"delta","delta":"Hello"}

data: {"type":"reasoning","reasoning":"Let me think about this..."}

data: {"type":"tool_call","tool_call":{"id":"call_1","name":"search","arguments":{"q":"x"}}}

data: {"type":"done","finish_reason":"tool_calls"}

```

Event types: `delta` (visible text), `reasoning` (thinking/reasoning text, rendered in a collapsible Thinking disclosure), `tool_call` (model requests a tool), `done` (stream complete), `error`.

The frontend reads with `fetch` + `ReadableStream` (not `EventSource` — `EventSource` can't POST). On `tool_call`, the UI prompts the user; on approve it `POST /api/tools/call`s the tool, appends the result to the conversation, and re-`POST /api/chat`s to continue. On `done` or `error` the turn finalizes.

This protocol is intentionally **not** OpenAI-shaped — the host's `Complete` implementation is free to call OpenAI, Anthropic, a local llama.cpp, or anything else. The reference implementation in `llmrouter` does a loopback HTTP call to its own `/v1/chat/completions` and translates OpenAI's SSE format into webchat events (including `delta.reasoning_content` / `delta.reasoning` → `EventReasoning`).

## Personas

One TOML file per persona. The filename stem becomes a stable identifier; the `name` field is the display name. See `examples/personas/` for ready-to-use examples.

```toml
# examples/personas/coder.toml
name = "Code Assistant"
description = "Helps with code review, writing tests, and debugging."
system_prompt = """You are a senior software engineer with decades of experience.
Always prefer readable, maintainable code over clever one-liners.
When suggesting changes, explain WHY, not just WHAT."""

[params]
temperature     = 0.2     # randomness (0 = deterministic, 1 = creative)
top_p          = 0.95    # nucleus sampling threshold
top_k          = 40      # top-k sampling (llama.cpp / Ollama)
repeat_penalty  = 1.1     # penalise repeated tokens (llama.cpp / Ollama)
context_length  = 8192    # max context window in tokens
```

All fields are optional except `name` (which falls back to the filename stem if omitted). `default_model` is also optional — when omitted, the user picks a model from the picker. `[params]` is a free-form map merged into every completion request — standard OpenAI params (`temperature`, `top_p`, `max_tokens`, `frequency_penalty`, `presence_penalty`) and llama.cpp/Ollama params (`top_k`, `repeat_penalty`, `context_length`) are all forwarded to the host, which passes them through to the underlying API.

The directory is watched with `fsnotify`; adding or editing a persona takes effect on the next request without restarting the server.

When no `PersonasDir` is configured, the chat still works — a single built-in `Default` persona with no system prompt is offered.

## Slash commands

One Markdown file per command. The filename (minus `.md`) is the command name, lowercased. Use `$ARGUMENTS` in the body to splice whatever the user typed after the command. See `examples/commands/` for ready-to-use examples.

Commands support optional YAML frontmatter at the top of the file (Claude CLI style):

```markdown
<!-- examples/commands/review.md -->
---
description: Review code and suggest improvements
argument-hint: <paste-code-or-describe-changes>
---

Review the following code or changes. Focus on:

1. **Correctness** — bugs, edge cases, logic errors
2. **Readability** — naming, structure, comments

Code or description to review:

$ARGUMENTS
```

Frontmatter fields:
- `description` — shown in the slash command dropdown
- `argument-hint` — placeholder hint shown after the command name (e.g. `<github-handle>`)
- `allowed-tools` — comma-separated tool names to auto-approve for this session when the command runs (Claude CLI style; patterns like `Bash(git:*)` are stripped to just the tool name)

Rules:
- Filenames beginning with `_` are drafts (skipped).
- Names are restricted to letters, digits, `-`, `_` (so they stay unambiguous in chat input and shell-safe).
- `/Help` and `/help` both resolve — the name is lowercased before lookup.
- The directory is watched; commands hot-reload.

When no `CommandsDir` is configured, slash commands are disabled entirely. Typing `/anything` is then sent to the model as a literal user message.

## MCP prompts (slash commands from the MCP server)

MCP prompts merge into the same `/` namespace as file-based slash commands. The user invokes them identically — there's no separate `/prompt` prefix. File commands take precedence on name collision.

### How it works

When the user types `/explain concept=recursion level=simple`:

1. The frontend checks file-based commands first — no match for `explain`
2. Falls through to MCP prompts — finds `explain` with declared args `concept` (required) and `level` (optional)
3. Parses arguments: `key=value` pairs, or positional shorthand for single-arg prompts (`/summarise some text` → `{text: "some text"}`)
4. Calls `POST {prefix}/api/prompts/get` with the name and parsed args
5. The host's `GetPrompt` renders the prompt and returns messages
6. Those messages are injected into the conversation and a completion turn starts

### Autocomplete

Typing `/` shows a unified dropdown of file commands and MCP prompts. MCP prompts are marked with a purple bolt icon and show their argument names (e.g. `concept* level`) in the hint column. Arrow keys navigate, Enter/Tab selects.

### Discovery

- `/list-prompts` — renders an info card listing all available MCP prompts with their argument signatures
- `/list-resources` — renders an info card listing all available MCP resources with their URIs

### Prompt arguments

Arguments are parsed from the text after the command name:

```
/explain concept=recursion level=advanced
/summarise this is a long block of text that goes to the single "text" arg
/review code="print('hello')"
```

If the prompt declares exactly one argument and the input has no `=` signs, the entire remainder is assigned to that argument positionally. Otherwise, `key=value` pairs are extracted.

## MCP resources (attach context with @)

MCP resources are data the user can attach to a message as context for the model. They appear as attachment chips (like email attachments) on the user's message bubble — the raw content is hidden from the transcript but sent to the model as prefixed context.

### How it works

1. User types `@` in the composer — a green dropdown shows matching resource URIs
2. Selecting a resource reads it immediately via `POST {prefix}/api/resources/read`
3. The content is added to a pending-attachments tray below the textarea (green chips with remove buttons)
4. On send, each attachment's content is prepended to the user's message text:

```
Resource docs://readme.md:
<full resource content here>

<user's actual message text>
```

5. The model sees the full resource text; the transcript shows only the chip.

### Resource templates

Template resources (with `{var}` placeholders in the URI) appear in the dropdown marked as "template". The user types the variable part into the URI — e.g. selecting `greeting://{name}` and the frontend reads `greeting://Ada` (the `@` autocomplete matches on URI substring, so typing `@greet` or `@Ada` finds it).

### Multiple attachments

Multiple `@resource` selections can be attached to a single message. Each is rendered as a separate chip and its content is injected separately.

## Frontend

The chat UI lives in [`web/`](web):
- `web/src/chat.js` — Alpine data component (includes the markdown processor). Conversations persist to `sessionStorage`. The frontend **does not bundle its own Alpine or Tailwind** — it loads them from the host's bundled assets. This keeps webchat free of CDN dependencies and version skew.
- `examples/chat.html` — reference template that hosts copy into their own template tree (where Tailwind can scan it during build).

### Script-order gotcha

The host's bundle calls `Alpine.start()` synchronously at the bottom. `start()` immediately dispatches `alpine:init` — the only window in which `Alpine.data(...)` registrations are accepted. So `chat.js` must run BEFORE the host bundle. The shipped example template orders the two `<script defer>` tags accordingly; if you customise the template, preserve that order.

## Auth

Auth is entirely the host's responsibility. webchat takes an `AuthMiddleware func(http.Handler) http.Handler` in `Config` and wraps every handler with it. The host decides what auth means — session cookie, bearer token, mTLS, IP allow-list, anything. nil means no auth (rare; appropriate only for fully internal hosts).

The host's template provides whatever login/logout UI it wants — webchat's JS has no knowledge of auth endpoints.

## Testing

```
go test ./...
```

Coverage focuses on the protocol handlers (chat streaming, tool filtering, tool execution, prompt rendering, resource reading), the persona and command loaders (TOML parsing, hot reload, edge cases), and the SSE translator. The frontend is not unit-tested; verify changes manually.

## License

See [LICENSE.txt](LICENSE.txt).
