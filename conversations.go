package webchat

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleConversations handles GET /api/conversations — lists all
// conversation summaries (no messages). Uses ETag so the browser
// can skip parsing when nothing changed.
func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	list, err := s.cfg.History.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []ConversationSummary{}
	}
	writeJSONWithETag(w, r, list)
}

// handleConversation handles GET/PUT/DELETE /api/conversations/{id}.
func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	prefix := s.cfg.Prefix + "/api/conversations/"
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}

	switch r.Method {
	case http.MethodGet:
		conv, err := s.cfg.History.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, conv)

	case http.MethodPut:
		var conv StoredConversation
		if err := json.NewDecoder(r.Body).Decode(&conv); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body")
			return
		}
		conv.ID = id
		if err := s.cfg.History.Save(r.Context(), &conv); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if s.cfg.Events != nil {
			s.cfg.Events.Broadcast(ServerEvent{Type: "conversation_saved", ID: id})
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case http.MethodDelete:
		if err := s.cfg.History.Delete(r.Context(), id); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if s.cfg.Events != nil {
			s.cfg.Events.Broadcast(ServerEvent{Type: "conversation_deleted", ID: id})
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		writeError(w, http.StatusMethodNotAllowed, "GET, PUT, DELETE required")
	}
}
