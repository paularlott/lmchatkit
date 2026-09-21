package lmchatkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
)

// handlePersonas returns the persona snapshot. Always an array — a built-in
// Default persona is included even when no source is configured.
func (s *Server) handlePersonas(w http.ResponseWriter, r *http.Request) {
	personas := []Persona{{ID: "default", Name: "Default"}}
	if s.personas != nil {
		if got, err := s.personas.Personas(r.Context()); err == nil && len(got) > 0 {
			personas = got
		}
	}
	writeJSONWithETag(w, r, personas)
}

// handleCommands returns the slash-command snapshot including the rendered
// markdown body. Bodies are small (typical command file is <1KB) and the
// count is bounded by what fits in the source, so we ship them in the
// listing rather than adding a per-command endpoint.
//
// Always returns a JSON array, even when no source is configured — JSON
// null would force every client to defend against null in addition to empty.
func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	cmds := []SlashCommand{}
	if s.commands != nil {
		if got, err := s.commands.Commands(r.Context()); err == nil && len(got) > 0 {
			cmds = got
		}
	}
	writeJSONWithETag(w, r, cmds)
}

// handleModels proxies the host's model list.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.host.Models(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if models == nil {
		models = []Model{}
	}
	writeJSONWithETag(w, r, models)
}

// chatRequest is the body shape expected by POST /api/chat. The server
// derives the system prompt from the persona and builds the tool list
// from the host — the browser sends neither.
type chatRequest struct {
	Model     string                 `json:"model"`
	PersonaID string                 `json:"persona_id"`
	Messages  []Message              `json:"messages"`
	Params    map[string]interface{} `json:"params,omitempty"`
}

// toolCallRequest is the body shape for /api/tools/call.
type toolCallRequest struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	// Source distinguishes who's calling: "app" for a call a mounted MCP
	// Apps view makes of itself (mountAppView's postMessage bridge proxying
	// the view's own tools/call — see chat.js), empty/anything else for the
	// normal model-approved path. Both hit this same endpoint with the same
	// body shape otherwise, so this is the only signal handleCallTool has
	// to enforce _meta.ui.visibility (the spec's MUST that a view may only
	// call an "app"-visible tool, and the model only a "model"-visible
	// one) — see toolVisibilityAllows.
	Source string `json:"source,omitempty"`
	// Via names the tool that mounted the calling app view — set by
	// chat.js alongside Source: "app". Required to enforce cross-server
	// ownership (see toolSourceAllows): visibility alone can't tell that a
	// view mounted from one unnamespaced federated server's tool is
	// reaching into a *different* unnamespaced server's identically
	// "app"-visible tool, since nothing about the tool name says which
	// server it came from.
	Via string `json:"via,omitempty"`
}

// handleCallTool invokes a tool. The frontend calls this after the user
// confirms a tool call from the model's response, or (Source: "app") when a
// mounted MCP Apps view calls a tool of its own.
func (s *Server) handleCallTool(w http.ResponseWriter, r *http.Request) {
	var req toolCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	role := "model"
	if req.Source == "app" {
		role = "app"
	}

	// Intercept the virtual skill-retrieval tool — route to ReadResource
	// instead of the host's CallTool (the skill tool is not registered on
	// the MCP server). Model-role only, and deliberately placed after the
	// role derivation: the skill tool isn't in Host.ListTools either, so
	// toolVisibilityAllows' fail-closed unknown-name denial is exactly what
	// an app view (role "app") asking for it by name hits below — skills
	// are host-level documentation for the model, not something a sandboxed
	// view should be able to pull.
	if role != "app" {
		if result, handled := s.trySkillToolCall(r.Context(), req.Name, req.Arguments); handled {
			writeJSON(w, http.StatusOK, result)
			return
		}
	}

	if !s.toolVisibilityAllows(r.Context(), req.Name, role) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("tool %q is not callable from this context", req.Name))
		return
	}
	if role == "app" && !s.toolSourceAllows(r.Context(), req.Via, req.Name) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("tool %q does not belong to the calling view's server", req.Name))
		return
	}

	res, err := s.host.CallTool(r.Context(), req.Name, req.Arguments)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// toolVisibilityAllows reports whether the named tool's _meta.ui.visibility
// permits role ("model" or "app"). Looks the tool up in Host.ListTools'
// full, unfiltered result — chat.go's own model-facing tool list has
// already been through FilterToolsForModel, which would hide an app-only
// tool from a lookup against it too, defeating the very calls this is meant
// to allow. Fails closed: a ListTools error or a name it doesn't recognize
// both deny, rather than letting an unresolvable call fall through as
// allowed.
func (s *Server) toolVisibilityAllows(ctx context.Context, name, role string) bool {
	tools, err := s.host.ListTools(ctx)
	if err != nil {
		return false
	}
	for _, t := range tools {
		if t.Name == name {
			return visibilityAllows(t.Visibility, role)
		}
	}
	return false
}

// toolSourceAllows reports whether an app view mounted from tool viaTool
// may call tool name, by comparing which MCP server each belongs to (see
// [SourceScopedHost]). If the host doesn't track tool ownership — it
// doesn't implement SourceScopedHost, meaning it aggregates at most one
// non-federated MCP server and so has no cross-server ambiguity to guard
// against — this is a no-op allow. When the host does track ownership, this
// fails closed: an unresolvable viaTool or name (lookup error, unknown
// tool) denies the call rather than falling through as allowed.
func (s *Server) toolSourceAllows(ctx context.Context, viaTool, name string) bool {
	scoped, ok := s.host.(SourceScopedHost)
	if !ok {
		return true
	}
	viaSource, ok := scoped.ToolSource(ctx, viaTool)
	if !ok {
		return false
	}
	targetSource, ok := scoped.ToolSource(ctx, name)
	if !ok {
		return false
	}
	return viaSource == targetSource
}

// handleListPrompts proxies the host's prompt list.
func (s *Server) handleListPrompts(w http.ResponseWriter, r *http.Request) {
	prompts, err := s.host.ListPrompts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONWithETag(w, r, prompts)
}

// promptGetRequest is the body shape for /api/prompts/get.
type promptGetRequest struct {
	Name string            `json:"name"`
	Args map[string]string `json:"args,omitempty"`
}

// handleGetPrompt renders a prompt by name with arguments.
func (s *Server) handleGetPrompt(w http.ResponseWriter, r *http.Request) {
	var req promptGetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	res, err := s.host.GetPrompt(r.Context(), req.Name, req.Args)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleListResources proxies the host's resource list.
func (s *Server) handleListResources(w http.ResponseWriter, r *http.Request) {
	resources, err := s.host.ListResources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONWithETag(w, r, resources)
}

// resourceReadRequest is the body shape for /api/resources/read.
type resourceReadRequest struct {
	URI string `json:"uri"`
	// Via names the tool this resource read is scoped to: the tool that
	// mounted the requesting app view, for a resources/read proxied from
	// inside its sandboxed iframe (see chat.js's mountAppView), or the tool
	// whose _meta.ui.resourceUri this is, for the host's own post-tool-call
	// fetch (hydrateAppResource). When set, the resource must come from the
	// same MCP server as Via — enforced fail-closed via [SourceScopedHost].
	// Empty Via reads uri unrestricted, matching prior behavior; both
	// current chat.js call sites always set it.
	Via string `json:"via,omitempty"`
}

// handleReadResource reads a resource by URI, scoped to the same MCP
// server as req.Via's tool when set and the host supports it — otherwise
// (no Via, or a host with no ownership tracking) this is an unrestricted
// read, same as before per-tool scoping existed.
func (s *Server) handleReadResource(w http.ResponseWriter, r *http.Request) {
	var req resourceReadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.URI == "" {
		writeError(w, http.StatusBadRequest, "uri is required")
		return
	}

	scoped, ok := s.host.(SourceScopedHost)
	if !ok || req.Via == "" {
		res, err := s.host.ReadResource(r.Context(), req.URI)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	source, ok := scoped.ToolSource(r.Context(), req.Via)
	if !ok {
		writeError(w, http.StatusForbidden, fmt.Sprintf("tool %q is not recognized", req.Via))
		return
	}
	res, err := scoped.ReadResourceFromSource(r.Context(), source, req.URI)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleAsset serves a file from the embedded JS/CSS bundle. The bundle is
// tiny (no minification, no chunking). Assets are served at a stable URL with
// a content-hash ETag and must-revalidate caching, so the browser rechecks
// every load: an instant 304 when the asset is unchanged, or the new bytes
// the moment the binary is upgraded (no 24h-stale window).
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Path[len(s.cfg.Prefix)+len("/assets/"):]
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	data, err := assetsFS.ReadFile("web/dist/" + rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch extOf(rel) {
	case ".js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func extOf(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return name[i:]
		}
		if name[i] == '/' {
			break
		}
	}
	return ""
}

// writeJSON writes a JSON response with the standard helper. Inline rather
// than imported from admin so this package stays self-contained.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONWithETag marshals v to JSON, computes an ETag from the bytes,
// and checks the If-None-Match request header. If the client already has
// this version, returns 304 Not Modified with no body — saves bandwidth
// and client-side parsing on the post-completion refresh calls.
func writeJSONWithETag(w http.ResponseWriter, r *http.Request, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal failed")
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// writeError writes an error response in the conventional {error: ...} shape.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
