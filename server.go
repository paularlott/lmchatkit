package webchat

import (
	"context"
	"embed"
	"io"
	"net/http"
)

// Config configures a [Server] at mount time.
type Config struct {
	// Prefix is the URL prefix webchat mounts under, e.g. "/chat". Must be
	// non-empty. Routes are registered as Prefix+"/api/..." and
	// Prefix+"/assets/...". The host owns the Prefix page route itself —
	// see webchat/examples/chat.html.
	Prefix string

	// PersonasDir is the directory scanned for persona TOML files. Empty
	// means no persona files — only the built-in Default persona is offered.
	//
	// Ignored when PersonaSource is set; use whichever fits the host. File
	// watching only applies to this dir, not to a custom source.
	PersonasDir string

	// CommandsDir is the directory scanned for slash-command markdown files.
	// Empty disables slash commands entirely. Ignored when CommandSource is
	// set.
	CommandsDir string

	// PersonaSource overrides PersonasDir. Use this when personas come from
	// somewhere other than the filesystem — typically a database, or a single
	// system-defined persona via [StaticPersonas].
	PersonaSource PersonaSource

	// CommandSource overrides CommandsDir. Same contract as PersonaSource.
	CommandSource CommandSource

	// Host is the contract between webchat and the embedding application.
	// Must be non-nil.
	Host Host

	// AuthMiddleware wraps every webchat HTTP handler. It is the host's
	// responsibility to enforce authentication, sessions, rate limiting, etc.
	// nil means no auth (rare; only appropriate for fully internal hosts).
	AuthMiddleware func(http.Handler) http.Handler

	// History persists chat conversations server-side. nil = browser
	// sessionStorage (no persistence across browser restarts, no
	// cross-tab sync). When non-nil, conversation CRUD endpoints are
	// mounted and the browser switches to server-side mode automatically.
	History HistoryStore

	// Events broadcasts changes to connected SSE clients for cross-tab
	// sync and push notifications (tools/prompts/resources changed).
	// nil = no SSE (browser falls back to polling on chat completion).
	Events *EventBroadcaster
}

// Server is a self-contained chat UI + backend. Build one with [New] and
// mount it into any *http.ServeMux via [Server.Mount].
type Server struct {
	cfg             Config
	personas        PersonaSource
	personasCloser  io.Closer // non-nil when we own a file-backed personaStore
	commands        CommandSource
	commandsCloser  io.Closer // non-nil when we own a file-backed commandStore
	host            Host
}

// New builds a Server, eagerly loading personas and slash commands from the
// configured sources so the first request is fast.
func New(cfg Config) (*Server, error) {
	if cfg.Prefix == "" {
		cfg.Prefix = "/chat"
	}
	if cfg.Host == nil {
		return nil, ErrMissingHost
	}

	s := &Server{cfg: cfg, host: cfg.Host}

	// Resolve persona source: explicit > dir > none.
	switch {
	case cfg.PersonaSource != nil:
		s.personas = cfg.PersonaSource
	case cfg.PersonasDir != "":
		store, err := newPersonaStore(cfg.PersonasDir)
		if err != nil {
			return nil, err
		}
		s.personas = store
		s.personasCloser = store
	}
	// Resolve command source.
	switch {
	case cfg.CommandSource != nil:
		s.commands = cfg.CommandSource
	case cfg.CommandsDir != "":
		store, err := newCommandStore(cfg.CommandsDir)
		if err != nil {
			return nil, err
		}
		s.commands = store
		s.commandsCloser = store
	}
	return s, nil
}

// Close releases watchers and goroutines owned by this Server. Sources the
// host supplied via Config.PersonaSource / Config.CommandSource are NOT
// closed — the host owns their lifecycle. Safe to call multiple times.
func (s *Server) Close() {
	if s.personasCloser != nil {
		s.personasCloser.Close()
	}
	if s.commandsCloser != nil {
		s.commandsCloser.Close()
	}
}

// ErrMissingHost is returned by [New] when Config.Host is nil.
var ErrMissingHost = errString("webchat: Config.Host is required")

// errString is a tiny error type so we get a sentinel with a stable message
// without pulling in fmt or errors just for one declaration.
type errString string

func (e errString) Error() string { return string(e) }

// Mount registers webchat's API and asset routes on mux under the
// configured prefix. It deliberately does NOT register a page route —
// the host owns the chat page template (so it lives in the host's
// source tree where Tailwind can scan it). Hosts render /chat
// themselves using the example template shipped at
// webchat/web/templates/chat.html as a starting point.
//
// Routes registered:
//   - {prefix}/api/...   — chat/tools/prompts/resources endpoints
//   - {prefix}/assets/... — embedded chat.js, chat.css, markdown.js
//
// The host's AuthMiddleware (if set) wraps every handler.
func (s *Server) Mount(mux *http.ServeMux) {
	wrap := s.cfg.AuthMiddleware
	if wrap == nil {
		wrap = func(h http.Handler) http.Handler { return h }
	}
	prefix := s.cfg.Prefix
	wrapf := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			wrap(h).ServeHTTP(w, r)
		}
	}

	// API
	mux.HandleFunc(prefix+"/api/personas", wrapf(s.handlePersonas))
	mux.HandleFunc(prefix+"/api/commands", wrapf(s.handleCommands))
	mux.HandleFunc(prefix+"/api/models", wrapf(s.handleModels))
	mux.HandleFunc(prefix+"/api/chat", wrapf(s.handleChat))
	mux.HandleFunc(prefix+"/api/tools", wrapf(s.handleListTools))
	mux.HandleFunc(prefix+"/api/tools/call", wrapf(s.handleCallTool))
	mux.HandleFunc(prefix+"/api/prompts", wrapf(s.handleListPrompts))
	mux.HandleFunc(prefix+"/api/prompts/get", wrapf(s.handleGetPrompt))
	mux.HandleFunc(prefix+"/api/resources", wrapf(s.handleListResources))
	mux.HandleFunc(prefix+"/api/resources/read", wrapf(s.handleReadResource))

	// Static assets (chat.js, chat.css, markdown.js bundles).
	mux.HandleFunc(prefix+"/assets/", wrapf(s.handleAsset))

	// Conversation CRUD — only mounted when a HistoryStore is configured.
	if s.cfg.History != nil {
		mux.HandleFunc(prefix+"/api/conversations", wrapf(s.handleConversations))
		mux.HandleFunc(prefix+"/api/conversations/", wrapf(s.handleConversation))
	}

	// SSE event stream — only mounted when an EventBroadcaster is configured.
	if s.cfg.Events != nil {
		mux.HandleFunc(prefix+"/api/events", wrapf(s.handleEvents))
	}
}

// hostCtxKey is a context key for stashing the authenticated user info the
// host's AuthMiddleware attaches. Reserved for future per-user logic.
type hostCtxKey struct{}

// withUser attaches a user identity into the request context. Hosts may call
// this from their AuthMiddleware so handlers can read it via [userFrom].
func withUser(r *http.Request, user any) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), hostCtxKey{}, user))
}

// userFrom extracts a previously-attached user. Returns nil if none.
func userFrom(r *http.Request) any {
	return r.Context().Value(hostCtxKey{})
}

//go:embed web/src/chat.js
var assetsFS embed.FS

// AssetFS exposes the bundled chat.js so hosts can serve it from their
// own asset pipeline if they prefer. [Server.Mount] already wires it up
// at {prefix}/assets/chat.js.
var AssetFS = assetsFS
