package webchat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
}

// handleCallTool invokes a tool. The frontend calls this after the user
// confirms a tool call from the model's response.
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

	// Intercept the virtual skill-retrieval tool — route to ReadResource
	// instead of the host's CallTool (the skill tool is not registered
	// on the MCP server).
	if result, handled := s.trySkillToolCall(r.Context(), req.Name, req.Arguments); handled {
		writeJSON(w, http.StatusOK, result)
		return
	}

	res, err := s.host.CallTool(r.Context(), req.Name, req.Arguments)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
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
}

// handleReadResource reads a resource by URI.
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
	res, err := s.host.ReadResource(r.Context(), req.URI)
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
	data, err := assetsFS.ReadFile("web/src/" + rel)
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
