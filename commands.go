package webchat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/fsnotify/fsnotify"
)

// SlashCommand is one user-invokable slash command loaded from a markdown
// file in CommandsDir. The command name is the filename stem (e.g. help.md ->
// /help). The Body is the raw markdown with optional $ARGUMENTS placeholder
// substituted at render time. Description and ArgumentHint come from YAML
// frontmatter at the top of the file (Claude-style):
//
//	---
//	description: Open a PR and tag a reviewer
//	argument-hint: <github-handle>
//	---
//
// Review the changes by $ARGUMENTS and suggest...
type SlashCommand struct {
	ID           string `json:"id"`                      // stable hash of name
	Name         string `json:"name"`                    // canonical name (filename stem), no leading slash
	Description  string `json:"description,omitempty"`   // from frontmatter, shown in the selection menu
	ArgumentHint string `json:"argument_hint,omitempty"` // from frontmatter, e.g. "<github-handle>"
	AllowedTools string `json:"allowed_tools,omitempty"` // from frontmatter, comma-separated tool names to auto-allow
	Body         string `json:"body"`                    // raw markdown (frontmatter stripped); $ARGUMENTS replaced when rendered
	Source       string `json:"source"`                  // short source hint for debugging
}

// commandStore is the watchable slash-command cache.
type commandStore struct {
	dir     string
	mu      sync.RWMutex
	current []SlashCommand
	watcher *fsnotify.Watcher
	done    chan struct{}
	wg      sync.WaitGroup
}

func newCommandStore(dir string) (*commandStore, error) {
	s := &commandStore{dir: dir, done: make(chan struct{})}
	if err := s.reload(); err != nil {
		return nil, err
	}
	if dir == "" {
		return s, nil
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("command watcher: %w", err)
	}
	s.watcher = w
	if err := w.Add(dir); err != nil {
		w.Close()
		s.watcher = nil
	} else {
		s.wg.Add(1)
		go s.watchLoop()
	}
	return s, nil
}

func (s *commandStore) snapshot() []SlashCommand {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SlashCommand, len(s.current))
	copy(out, s.current)
	return out
}

// Commands satisfies [CommandSource].
func (s *commandStore) Commands(ctx context.Context) ([]SlashCommand, error) {
	return s.snapshot(), nil
}

func (s *commandStore) close() {
	close(s.done)
	if s.watcher != nil {
		s.watcher.Close()
	}
	s.wg.Wait()
}

// Close satisfies io.Closer.
func (s *commandStore) Close() error { s.close(); return nil }

func (s *commandStore) watchLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case _, ok := <-s.watcher.Events:
			if !ok {
				return
			}
			// Debounce: any change reloads everything.
			_ = s.reload()
		case _, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

// reload re-reads every .md file in dir. Filenames beginning with _ are
// treated as drafts and skipped. Missing/unreadable dir is treated as empty.
func (s *commandStore) reload() error {
	var out []SlashCommand
	if s.dir != "" {
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			s.mu.Lock()
			s.current = out
			s.mu.Unlock()
			return err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			stem := strings.TrimSuffix(e.Name(), ".md")
			if strings.HasPrefix(stem, "_") {
				continue
			}
			if !isValidCommandName(stem) {
				continue
			}
			body, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
			if err != nil {
				continue
			}
			meta, mdBody := parseFrontmatter(string(body))
			out = append(out, SlashCommand{
				ID:           stableID(stem),
				Name:         stem,
				Description:  meta["description"],
				ArgumentHint: meta["argument-hint"],
				AllowedTools: meta["allowed-tools"],
				Body:         mdBody,
				Source:       e.Name(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	s.mu.Lock()
	s.current = out
	s.mu.Unlock()
	return nil
}

// parseFrontmatter extracts simple YAML key:value pairs from a `---` delimited
// block at the top of a markdown file (Claude CLI slash-command style).
// Returns the metadata map and the body with the frontmatter block stripped.
// If no frontmatter is present, returns an empty map and the original content.
func parseFrontmatter(content string) (meta map[string]string, body string) {
	meta = map[string]string{}
	trimmed := strings.TrimLeft(content, "\n\r ")
	if !strings.HasPrefix(trimmed, "---") {
		return meta, content
	}
	lines := strings.Split(trimmed, "\n")
	endIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			endIdx = i
			break
		}
	}
	if endIdx == -1 {
		return meta, content
	}
	for _, line := range lines[1:endIdx] {
		idx := strings.Index(line, ":")
		if idx == -1 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		meta[key] = val
	}
	body = strings.TrimSpace(strings.Join(lines[endIdx+1:], "\n"))
	return meta, body
}

// isValidCommandName restricts filenames to lowercase ascii / digits / hyphen
// so /-commands stay unambiguous and shell-safe. Uppercase is allowed for
// common conventions (/HELP).
func isValidCommandName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// render substitutes $ARGUMENTS in the command body with the given argument
// string (which may be empty). The substitution is literal — no shell quoting
// is applied — so the resulting message reaches the model verbatim.
func (c SlashCommand) render(arguments string) string {
	return strings.ReplaceAll(c.Body, "$ARGUMENTS", arguments)
}

// commandName extracts the command name (lowercased, no slash) from a user
// message beginning with "/", plus the trailing argument string. Returns
// ok=false if the input is not a slash command. The command name is
// lowercased so /Help and /help both resolve.
func parseSlashInput(input string) (name string, arguments string, ok bool) {
	s := strings.TrimSpace(input)
	if !strings.HasPrefix(s, "/") || len(s) == 1 {
		return "", "", false
	}
	rest := s[1:]
	// Split on first whitespace.
	idx := strings.IndexAny(rest, " \t\n")
	if idx == -1 {
		return strings.ToLower(rest), "", true
	}
	return strings.ToLower(rest[:idx]), strings.TrimSpace(rest[idx:]), true
}