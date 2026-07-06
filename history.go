package webchat

import (
	"context"
	"sync"
)

// HistoryStore persists chat conversations server-side. If nil on Config,
// the browser uses sessionStorage (no server-side persistence, no
// cross-tab sync). Hosts implement this with KV stores, databases,
// files — whatever fits. For per-user stores (knot), the implementation
// extracts the user from the context.
type HistoryStore interface {
	List(ctx context.Context) ([]ConversationSummary, error)
	Get(ctx context.Context, id string) (*StoredConversation, error)
	Save(ctx context.Context, conv *StoredConversation) error
	Delete(ctx context.Context, id string) error
}

// ConversationSummary is the lightweight sidebar entry — no messages.
type ConversationSummary struct {
	ID        string                 `json:"id"`
	Title     string                 `json:"title"`
	PersonaID string                 `json:"persona_id"`
	Model     string                 `json:"model"`
	Params    map[string]interface{} `json:"params,omitempty"`
	CreatedAt int64                  `json:"created_at"`
	UpdatedAt int64                  `json:"updated_at"`
}

// StoredConversation is a full conversation including messages.
type StoredConversation struct {
	ConversationSummary
	Messages     []Message `json:"messages"`
	EnabledTools []string  `json:"enabled_tools,omitempty"`
}

// ServerEvent is one event pushed to SSE subscribers.
type ServerEvent struct {
	Type string `json:"type"` // conversation_saved, conversation_deleted, conversation_renamed, prompts_changed, resources_changed
	ID   string `json:"id,omitempty"`
}

// EventBroadcaster fans out events to all connected SSE subscribers.
// Used for cross-tab sync (conversation saved/deleted from another
// tab) and push notifications (tools/prompts/resources changed by the
// scriptling watcher).
type EventBroadcaster struct {
	mu          sync.RWMutex
	subscribers map[chan ServerEvent]struct{}
}

// NewEventBroadcaster creates a ready-to-use broadcaster.
func NewEventBroadcaster() *EventBroadcaster {
	return &EventBroadcaster{subscribers: make(map[chan ServerEvent]struct{})}
}

// Subscribe returns a channel that receives events. The caller must
// Unsubscribe when done (e.g. when the SSE connection closes).
func (b *EventBroadcaster) Subscribe() chan ServerEvent {
	ch := make(chan ServerEvent, 16)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber and closes its channel.
func (b *EventBroadcaster) Unsubscribe(ch chan ServerEvent) {
	b.mu.Lock()
	delete(b.subscribers, ch)
	b.mu.Unlock()
	close(ch)
}

// Broadcast sends an event to all subscribers. Non-blocking — if a
// subscriber's buffer is full, the event is dropped.
func (b *EventBroadcaster) Broadcast(event ServerEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}
