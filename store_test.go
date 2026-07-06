package webchat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPersonaStoreAlwaysIncludesDefault(t *testing.T) {
	s, err := newPersonaStore("")
	if err != nil {
		t.Fatalf("newPersonaStore: %v", err)
	}
	defer s.close()
	got := s.snapshot()
	if len(got) != 1 || got[0].ID != "default" || got[0].Name != "Default" {
		t.Fatalf("expected only Default persona, got %+v", got)
	}
}

// TestPersonaSourceInterface confirms personaStore satisfies PersonaSource at
// the type level — every source the host can pass to Config.PersonaSource
// must implement this contract.
func TestPersonaSourceInterface(t *testing.T) {
	var _ PersonaSource = (*personaStore)(nil)
	var _ PersonaSource = StaticPersonas{}
}

// TestCommandSourceInterface same for commands.
func TestCommandSourceInterface(t *testing.T) {
	var _ CommandSource = (*commandStore)(nil)
	var _ CommandSource = StaticCommands{}
}

// TestStaticPersonasSource verifies the slice-backed PersonaSource used for
// single-tenant hosts (e.g. knot's one system-defined persona).
func TestStaticPersonasSource(t *testing.T) {
	src := StaticPersonas{
		{ID: "knot", Name: "Knot", SystemPrompt: "be helpful", DefaultModel: "knot-1"},
	}
	got, err := src.Personas(context.Background())
	if err != nil {
		t.Fatalf("Personas: %v", err)
	}
	if len(got) != 1 || got[0].ID != "knot" {
		t.Fatalf("got %+v", got)
	}
}

// TestStaticCommandsSource verifies the slice-backed CommandSource.
func TestStaticCommandsSource(t *testing.T) {
	src := StaticCommands{
		{Name: "help", Body: "Help is here. $ARGUMENTS"},
	}
	got, err := src.Commands(context.Background())
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	if len(got) != 1 || got[0].Name != "help" {
		t.Fatalf("got %+v", got)
	}
}

// TestServerUsesCustomPersonaSource verifies Config.PersonaSource overrides
// Config.PersonasDir and is consulted live on every request (so DB-backed
// sources reflect the current row set without a watcher).
func TestServerUsesCustomPersonaSource(t *testing.T) {
	src := &mutablePersonas{items: []Persona{{ID: "p1", Name: "First"}}}
	srv, err := New(Config{
		Prefix:        "/chat",
		PersonaSource: src,
		Host:          &fakeHost{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()

	first := personasViaHTTP(t, srv)
	if len(first) != 1 || first[0].ID != "p1" {
		t.Fatalf("first request: %+v", first)
	}

	// Mutate the source's backing data — no rebuild, no watcher ping; the
	// next request must see the new set.
	src.items = []Persona{{ID: "p2", Name: "Second"}}
	second := personasViaHTTP(t, srv)
	if len(second) != 1 || second[0].ID != "p2" {
		t.Fatalf("second request: %+v (expected live refresh)", second)
	}
}

// TestServerUsesCustomCommandSource mirrors the persona test for commands.
func TestServerUsesCustomCommandSource(t *testing.T) {
	src := &mutableCommands{items: []SlashCommand{{Name: "c1", Body: "one"}}}
	srv, err := New(Config{
		Prefix:        "/chat",
		CommandSource: src,
		Host:          &fakeHost{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()

	first := commandsViaHTTP(t, srv)
	if len(first) != 1 || first[0].Name != "c1" {
		t.Fatalf("first request: %+v", first)
	}

	src.items = []SlashCommand{{Name: "c2", Body: "two"}}
	second := commandsViaHTTP(t, srv)
	if len(second) != 1 || second[0].Name != "c2" {
		t.Fatalf("second request: %+v (expected live refresh)", second)
	}
}

// mutablePersonas / mutableCommands are tiny test doubles that let us swap
// the underlying slice between requests to prove the source is queried per
// request rather than cached at Server.New time.
type mutablePersonas struct{ items []Persona }

func (m *mutablePersonas) Personas(ctx context.Context) ([]Persona, error) {
	return m.items, nil
}

type mutableCommands struct{ items []SlashCommand }

func (m *mutableCommands) Commands(ctx context.Context) ([]SlashCommand, error) {
	return m.items, nil
}

func personasViaHTTP(t *testing.T, srv *Server) []Persona {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/chat/api/personas", nil)
	rec := httptest.NewRecorder()
	srv.handlePersonas(rec, req)
	var got []Persona
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	return got
}

func commandsViaHTTP(t *testing.T, srv *Server) []SlashCommand {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/chat/api/commands", nil)
	rec := httptest.NewRecorder()
	srv.handleCommands(rec, req)
	var got []SlashCommand
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	return got
}

func TestPersonaStoreLoadsTOMLFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "coder.toml"), []byte(`
name = "Coder"
description = "Codes things"
system_prompt = "You write code."
default_model = "gpt-4"

[params]
temperature = 0.2
max_tokens = 4096
`))
	write(t, filepath.Join(dir, "writer.toml"), []byte(`
name = "Writer"
`))
	// non-toml is ignored
	write(t, filepath.Join(dir, "notes.md"), []byte(`# not a persona`))

	s, err := newPersonaStore(dir)
	if err != nil {
		t.Fatalf("newPersonaStore: %v", err)
	}
	defer s.close()

	got := s.snapshot()
	// Default + Coder + Writer = 3
	if len(got) != 3 {
		t.Fatalf("expected 3 personas, got %d (%+v)", len(got), got)
	}

	var coder Persona
	for _, p := range got {
		if p.Name == "Coder" {
			coder = p
		}
	}
	if coder.ID == "" {
		t.Fatal("Coder not found")
	}
	if coder.Description != "Codes things" || coder.SystemPrompt != "You write code." || coder.DefaultModel != "gpt-4" {
		t.Fatalf("Coder metadata wrong: %+v", coder)
	}
	if coder.Params["temperature"] != 0.2 {
		t.Fatalf("Coder params.temperature wrong: %v", coder.Params["temperature"])
	}
}

func TestPersonaStoreReloadPicksUpNewFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := newPersonaStore(dir)
	if err != nil {
		t.Fatalf("newPersonaStore: %v", err)
	}
	defer s.close()

	if got := len(s.snapshot()); got != 1 {
		t.Fatalf("expected only Default initially, got %d", got)
	}

	// Add a new persona file and force a reload.
	write(t, filepath.Join(dir, "new.toml"), []byte(`name = "New"`))
	if err := s.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := s.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected Default+New after reload, got %d (%+v)", len(got), got)
	}
}

func TestPersonaStableIDIsDeterministic(t *testing.T) {
	a := stableID("coder")
	b := stableID("coder")
	c := stableID("writer")
	if a != b {
		t.Fatalf("same stem should produce same ID: %s vs %s", a, b)
	}
	if a == c {
		t.Fatalf("different stems should produce different IDs")
	}
}

func TestCommandStoreEmptyByDefault(t *testing.T) {
	s, err := newCommandStore("")
	if err != nil {
		t.Fatalf("newCommandStore: %v", err)
	}
	defer s.close()
	if got := s.snapshot(); len(got) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
}

func TestCommandStoreLoadsMarkdown(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "help.md"), []byte(`Available commands: /clear, /model. Args: $ARGUMENTS`))
	write(t, filepath.Join(dir, "summarize.md"), []byte(`Summarize this: $ARGUMENTS`))
	// _drafts skipped
	write(t, filepath.Join(dir, "_draft.md"), []byte(`draft`))
	// non-md skipped
	write(t, filepath.Join(dir, "notes.txt"), []byte(`text`))
	// invalid name skipped
	write(t, filepath.Join(dir, "bad name.md"), []byte(`space`))

	s, err := newCommandStore(dir)
	if err != nil {
		t.Fatalf("newCommandStore: %v", err)
	}
	defer s.close()

	got := s.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 commands, got %d (%+v)", len(got), got)
	}

	var help SlashCommand
	for _, c := range got {
		if c.Name == "help" {
			help = c
		}
	}
	if help.Name != "help" {
		t.Fatalf("help command not found")
	}
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}
