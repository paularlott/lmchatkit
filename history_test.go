package lmchatkit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// memoryHistoryTestStore is a minimal HistoryStore for testing the
// conversation handlers without snapshotkv.
type memoryHistoryTestStore struct {
	data map[string]*StoredConversation
}

func (m *memoryHistoryTestStore) List(ctx context.Context) ([]ConversationSummary, error) {
	out := make([]ConversationSummary, 0, len(m.data))
	for _, c := range m.data {
		out = append(out, c.ConversationSummary)
	}
	return out, nil
}

func (m *memoryHistoryTestStore) Get(ctx context.Context, id string) (*StoredConversation, error) {
	c, ok := m.data[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return c, nil
}

func (m *memoryHistoryTestStore) Save(ctx context.Context, conv *StoredConversation) error {
	m.data[conv.ID] = conv
	return nil
}

func (m *memoryHistoryTestStore) Delete(ctx context.Context, id string) error {
	delete(m.data, id)
	return nil
}

func TestConversationCRUD(t *testing.T) {
	store := &memoryHistoryTestStore{data: make(map[string]*StoredConversation)}
	s := newTestServer(t, &fakeHost{})
	s.cfg.History = store

	// PUT a conversation
	conv := StoredConversation{
		ConversationSummary: ConversationSummary{
			ID: "conv-1", Title: "Test", PersonaID: "default", Model: "m1",
		},
		Messages: []Message{{Role: "user", Content: "hello"}},
	}
	body, _ := json.Marshal(conv)
	req := httptest.NewRequest(http.MethodPut, "/chat/api/conversations/conv-1", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.handleConversation(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}

	// GET it back
	req = httptest.NewRequest(http.MethodGet, "/chat/api/conversations/conv-1", nil)
	rec = httptest.NewRecorder()
	s.handleConversation(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: want 200 got %d", rec.Code)
	}
	var got StoredConversation
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Title != "Test" || len(got.Messages) != 1 {
		t.Fatalf("unexpected conversation: %+v", got)
	}

	// LIST
	req = httptest.NewRequest(http.MethodGet, "/chat/api/conversations", nil)
	rec = httptest.NewRecorder()
	s.handleConversations(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST: want 200 got %d", rec.Code)
	}
	var list []ConversationSummary
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].ID != "conv-1" {
		t.Fatalf("unexpected list: %+v", list)
	}

	// DELETE
	req = httptest.NewRequest(http.MethodDelete, "/chat/api/conversations/conv-1", nil)
	rec = httptest.NewRecorder()
	s.handleConversation(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: want 204 got %d", rec.Code)
	}

	// Verify gone
	if _, err := store.Get(context.Background(), "conv-1"); err == nil {
		t.Fatal("conversation should be deleted")
	}
}

func TestEventBroadcaster(t *testing.T) {
	b := NewEventBroadcaster()
	ch1 := b.Subscribe()
	ch2 := b.Subscribe()

	b.Broadcast(ServerEvent{Type: "tools_changed"})

	ev1 := <-ch1
	ev2 := <-ch2
	if ev1.Type != "tools_changed" || ev2.Type != "tools_changed" {
		t.Fatalf("expected tools_changed, got %s / %s", ev1.Type, ev2.Type)
	}

	// Unsubscribe ch1, broadcast again — ch2 should still receive.
	b.Unsubscribe(ch1)
	b.Broadcast(ServerEvent{Type: "prompts_changed"})

	ev2 = <-ch2
	if ev2.Type != "prompts_changed" {
		t.Fatalf("expected prompts_changed, got %s", ev2.Type)
	}

	// ch1 is closed — receiving should yield zero value immediately.
	if _, ok := <-ch1; ok {
		t.Fatal("ch1 should be closed after Unsubscribe")
	}
}

func TestEventBroadcasterDropsOnFullBuffer(t *testing.T) {
	b := NewEventBroadcaster()
	ch := b.Subscribe()

	// Fill the buffer (capacity 16) without reading.
	for i := 0; i < 20; i++ {
		b.Broadcast(ServerEvent{Type: "test"})
	}

	// Should not block. Read what we can — at least the buffer capacity.
	count := 0
	for range ch {
		count++
		if count >= 16 {
			break
		}
	}
	if count < 16 {
		t.Fatalf("expected at least 16 events, got %d", count)
	}
}
