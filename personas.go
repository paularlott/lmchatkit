package lmchatkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	cli "github.com/paularlott/cli"
	cli_toml "github.com/paularlott/cli/toml"
)

// Persona is one chat persona loaded from a TOML file in PersonasDir.
// SystemPrompt, DefaultModel and Params are all optional; an empty persona
// is valid and reduces to a no-op preset.
type Persona struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Description   string                 `json:"description,omitempty"`
	SystemPrompt  string                 `json:"system_prompt,omitempty"`
	DefaultModel  string                 `json:"default_model,omitempty"`
	Params        map[string]interface{} `json:"params,omitempty"`

	// SourceFile is absolute path the persona was loaded from. Empty for the
	// built-in "Default" persona.
	SourceFile string `json:"-"`
}

// personaStore is the watchable persona cache. It reloads on file changes
// (so users can iterate on personas without restarting the server) and
// returns a snapshot read-only to callers.
type personaStore struct {
	dir      string
	mu       sync.RWMutex
	current  []Persona
	watcher  *fsnotify.Watcher
	done     chan struct{}
	wg       sync.WaitGroup
}

func newPersonaStore(dir string) (*personaStore, error) {
	s := &personaStore{dir: dir, done: make(chan struct{})}
	if err := s.reload(); err != nil {
		return nil, err
	}
	if dir == "" {
		return s, nil
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("persona watcher: %w", err)
	}
	s.watcher = w
	if err := w.Add(dir); err != nil {
		w.Close()
		s.watcher = nil
		// Watching is best-effort; live reload just won't work. Don't fail
		// startup over it.
	} else {
		s.wg.Add(1)
		go s.watchLoop()
	}
	return s, nil
}

func (s *personaStore) snapshot() []Persona {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Persona, len(s.current))
	copy(out, s.current)
	return out
}

// Personas satisfies [PersonaSource]. The persona store is read on every
// call so a DB-backed replacement sees identical semantics.
func (s *personaStore) Personas(ctx context.Context) ([]Persona, error) {
	return s.snapshot(), nil
}

func (s *personaStore) close() {
	close(s.done)
	if s.watcher != nil {
		s.watcher.Close()
	}
	s.wg.Wait()
}

// Close satisfies io.Closer so Server can treat file-backed and host-supplied
// sources uniformly. Internal close is idempotent enough for our use.
func (s *personaStore) Close() error { s.close(); return nil }

func (s *personaStore) watchLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case ev, ok := <-s.watcher.Events:
			if !ok {
				return
			}
			if filepath.Ext(ev.Name) != ".toml" {
				continue
			}
			_ = s.reload()
		case _, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

// reload re-reads every .toml in s.dir and replaces the in-memory snapshot.
// Missing dir is treated as empty (the Default persona is still present).
// Symlinks are followed by os.ReadDir / cli_toml.
func (s *personaStore) reload() error {
	out := []Persona{{ID: "default", Name: "Default"}}

	if s.dir != "" {
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			// Missing/unreadable dir: keep just Default. Surface the error
			// only on initial load (handled by newPersonaStore caller).
			s.mu.Lock()
			s.current = out
			s.mu.Unlock()
			return err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
				continue
			}
			path := filepath.Join(s.dir, e.Name())
			p, err := loadPersonaFile(path)
			if err != nil {
				continue // skip malformed persona rather than failing the lot
			}
			out = append(out, p)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	s.mu.Lock()
	s.current = out
	s.mu.Unlock()
	return nil
}

// loadPersonaFile parses one persona TOML. The ID is derived from the
// filename stem so personas can be referenced from saved conversations even
// if their display name changes.
func loadPersonaFile(path string) (Persona, error) {
	base := cli_toml.NewConfigFile(&path, func() []string { return []string{filepath.Dir(path)} })
	cfg := cli.NewTypedConfigFile(base)
	if err := cfg.LoadData(); err != nil {
		return Persona{}, fmt.Errorf("parse %s: %w", path, err)
	}
	stem := strings.TrimSuffix(filepath.Base(path), ".toml")
	p := Persona{
		ID:           stableID(stem),
		Name:         cfg.GetString("name"),
		Description:  cfg.GetString("description"),
		SystemPrompt: cfg.GetString("system_prompt"),
		DefaultModel: cfg.GetString("default_model"),
		SourceFile:   path,
	}
	if p.Name == "" {
		p.Name = stem
	}
	if v, ok := cfg.GetValue("params"); ok {
		if m, ok := v.(map[string]any); ok {
			p.Params = m
		}
	}
	if p.Params == nil {
		p.Params = map[string]interface{}{}
	}
	return p, nil
}

// stableID turns a filename stem into a short, stable identifier suitable for
// JSON references. We hash to keep IDs URL-safe even if the filename contains
// spaces or punctuation; first 8 hex chars is enough to disambiguate within a
// typical persona set.
func stableID(stem string) string {
	sum := sha256.Sum256([]byte(stem))
	return hex.EncodeToString(sum[:])[:8]
}
