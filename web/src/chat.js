// webchat — markdown processor.
//
// Ported from knot's web/src/js/components/chat.js (same author, same
// licence). Zero-dependency, handles: fenced + inline code with language
// tagging, GitHub-style tables (with separator-row header detection),
// nested ordered/unordered lists (different bullet/numbering styles per
// depth), nested blockquotes, h1-h6, **bold**, __bold__, *italic*, _italic_,
// ~~strikethrough~~, [text](url) links, and horizontal rules. Code blocks
// and inline code have their contents HTML-escaped so user/model-supplied
// HTML inside them is rendered as text, not executed.
//
// Globals are exposed on window so chat.js (loaded as a non-module script)
// can call them without ES imports. This keeps webchat's bundle friendly
// to hosts that don't ship a JS module system.

(function () {
  function processMarkdown(text) {
    if (!text) return '';

    // Protect code spans first so nothing inside them gets re-processed.
    const codeBlocks = [];
    const inlineCodeBlocks = [];

    text = text.replace(/```[\s\S]*?```/g, (match) => {
      const placeholder = `CODEBLOCK${codeBlocks.length}PLACEHOLDER`;
      codeBlocks.push(match);
      return placeholder;
    });

    text = text.replace(/`[^`\n]+`/g, (match) => {
      const placeholder = `INLINECODE${inlineCodeBlocks.length}PLACEHOLDER`;
      inlineCodeBlocks.push(match);
      return placeholder;
    });

    // Tables (multi-line, pipe-delimited, optional header separator row).
    text = text.replace(/^((?:\|.*\|(?:\s*\n|$))+)/gm, (match) => {
      return processTable(match);
    });

    // Now process everything else.
    text = text
      .trim()
      // Block quotes (> text) — supports multi-line, recursively formats
      // inner bold/italic/code/links.
      .replace(/^((?:>\s*.+(?:\n|$))+)/gm, (match) => {
        const lines = match.split('\n').filter((line) => line.trim());
        const content = lines.map((line) => line.replace(/^>\s?/, '')).join('\n');
        return `<blockquote class="md-blockquote">${processNestedMarkdown(content)}</blockquote>`;
      })
      // Lists (ordered and unordered, with nesting).
      .replace(/^((?:[ \t]*(?:\d+\.|\*|\+|\-)\s+.+(?:\n|$))+)/gm, (match) => {
        return processLists(match);
      })
      // Horizontal rules.
      .replace(/^---\s*$/gm, '<hr class="md-hr">')
      .replace(/^\*\*\*$/gm, '<hr class="md-hr">')
      // Headings — emit semantic classes; host CSS defines the look (size,
      // weight, colour, dark-mode variant). Keeps the processor independent
      // of the host's Tailwind build (those classes never get scanned, so
      // utility classes here would silently no-op — see the h3 dark-mode
      // bug).
      .replace(/^######\s+(.+)$/gm, '<h6 class="md-h6">$1</h6>')
      .replace(/^#####\s+(.+)$/gm, '<h5 class="md-h5">$1</h5>')
      .replace(/^####\s+(.+)$/gm, '<h4 class="md-h4">$1</h4>')
      .replace(/^###\s+(.+)$/gm, '<h3 class="md-h3">$1</h3>')
      .replace(/^##\s+(.+)$/gm, '<h2 class="md-h2">$1</h2>')
      .replace(/^#\s+(.+)$/gm, '<h1 class="md-h1">$1</h1>')
      // Strikethrough.
      .replace(/~~(.*?)~~/g, '<del>$1</del>')
      // Bold.
      .replace(/\*\*(.*?)\*\*/g, '<strong>$1</strong>')
      .replace(/__(.*?)__/g, '<strong>$1</strong>')
      // Italic.
      .replace(/\*([^*\s][^*]*[^*\s]|[^*\s])\*/g, '<em>$1</em>')
      .replace(/\b_([^_\s][^_]*[^_\s]|[^_\s])_\b/g, '<em>$1</em>')
      // Links.
      .replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" class="md-a" target="_blank" rel="noopener noreferrer">$1</a>')
      // Line breaks.
      .replace(/\n/g, '<br>');

    // Restore inline code (escaped).
    inlineCodeBlocks.forEach((code, index) => {
      const placeholder = `INLINECODE${index}PLACEHOLDER`;
      const codeContent = code.slice(1, -1);
      const replacement = `<code class="md-code">${escapeHtml(codeContent)}</code>`;
      text = text.replace(placeholder, replacement);
    });

    // Restore fenced code blocks (escaped, tagged with language).
    codeBlocks.forEach((code, index) => {
      const placeholder = `CODEBLOCK${index}PLACEHOLDER`;
      const match = code.match(/```(\w+)?\s*([\s\S]*?)\s*```/);
      if (match) {
        const [, lang, codeContent] = match;
        const language = lang || 'text';
        const replacement = `<pre class="md-pre"><code class="language-${language}">${escapeHtml(codeContent.trim())}</code></pre>`;
        text = text.replace(placeholder, replacement);
      }
    });

    return text;
  }

  // processNestedMarkdown is the inner formatter for blockquote + list-item
  // + table-cell content: bold, italic, inline code, links, line breaks.
  function processNestedMarkdown(text) {
    return text
      .replace(/\*\*(.*?)\*\*/g, '<strong>$1</strong>')
      .replace(/__(.*?)__/g, '<strong>$1</strong>')
      .replace(/\*([^*\s][^*]*[^*\s]|[^*\s])\*/g, '<em>$1</em>')
      .replace(/\b_([^_\s][^_]*[^_\s]|[^_\s])_\b/g, '<em>$1</em>')
      .replace(/`([^`]+)`/g, '<code class="md-code">$1</code>')
      .replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" class="md-a" target="_blank" rel="noopener noreferrer">$1</a>')
      .replace(/\n/g, '<br>');
  }

  // processLists handles nested ordered/unordered lists. Indent depth is
  // 2 spaces per level. Bullet/numbering style cycles by depth.
  function processLists(text) {
    const lines = text.split('\n').filter((line) => line.trim());
    const result = [];
    const stack = []; // {type, level}

    for (const line of lines) {
      const match = line.match(/^(\s*)(\d+\.|\*|\+|\-)\s+(.+)$/);
      if (!match) continue;

      const [, indent, marker, content] = match;
      const level = Math.floor(indent.length / 2);
      const isOrdered = /^\d+\./.test(marker);
      const listType = isOrdered ? 'ol' : 'ul';

      // Close lists at deeper or equal levels when moving shallower, or
      // when switching list types at the same level.
      while (stack.length > 0 &&
        (stack[stack.length - 1].level > level ||
          (stack[stack.length - 1].level === level && stack[stack.length - 1].type !== listType))) {
        const item = stack.pop();
        result.push(`</li></${item.type}>`);
      }

      if (stack.length === 0 || stack[stack.length - 1].level < level) {
        if (isOrdered) {
          const numberingStyles = ['decimal', 'lower-alpha', 'lower-roman', 'decimal'];
          const styleIndex = level % numberingStyles.length;
          result.push(`<${listType} class="md-list" style="list-style-type: ${numberingStyles[styleIndex]};">`);
        } else {
          const bulletStyles = ['disc', 'circle', 'square', 'disc'];
          const styleIndex = level % bulletStyles.length;
          result.push(`<${listType} class="md-list" style="list-style-type: ${bulletStyles[styleIndex]};">`);
        }
        stack.push({ type: listType, level });
      } else if (stack.length > 0 && stack[stack.length - 1].level === level) {
        result.push('</li>');
      }

      result.push(`<li>${processNestedMarkdown(content)}`);
    }

    while (stack.length > 0) {
      const item = stack.pop();
      result.push(`</li></${item.type}>`);
    }

    return result.join('');
  }

  // processTable parses a pipe-delimited block. A row of only dashes,
  // colons, and pipes following the first row marks it as a header.
  function processTable(text) {
    const lines = text.trim().split('\n').filter((line) => line.trim());
    if (lines.length < 2) return text;

    const tableRows = [];
    let hasHeader = false;

    for (let i = 0; i < lines.length; i++) {
      const line = lines[i].trim();
      if (!line.startsWith('|') || !line.endsWith('|')) continue;

      const content = line.slice(1, -1);

      if (/^[\s\-:|]+$/.test(content) && content.includes('-')) {
        if (tableRows.length === 1) {
          hasHeader = true;
        }
        continue;
      }

      const cells = content.split('|').map((cell) => cell.trim());
      tableRows.push(cells);
    }

    if (tableRows.length === 0) return text;

    let html = '<div class="md-table-wrap"><table class="md-table">';

    if (hasHeader && tableRows.length > 0) {
      html += '<thead class="md-thead"><tr>';
      for (const cell of tableRows[0]) {
        html += `<th class="md-th">${processNestedMarkdown(cell)}</th>`;
      }
      html += '</tr></thead>';

      if (tableRows.length > 1) {
        html += '<tbody>';
        for (let i = 1; i < tableRows.length; i++) {
          html += '<tr class="md-tr">';
          for (const cell of tableRows[i]) {
            html += `<td class="md-td">${processNestedMarkdown(cell)}</td>`;
          }
          html += '</tr>';
        }
        html += '</tbody>';
      }
    } else {
      html += '<tbody>';
      for (const row of tableRows) {
        html += '<tr class="md-tr">';
        for (const cell of row) {
          html += `<td class="md-td">${processNestedMarkdown(cell)}</td>`;
        }
        html += '</tr>';
      }
      html += '</tbody>';
    }

    html += '</table></div>';
    return html;
  }

  // escapeHtml is the canonical "render this string as text" helper —
  // anything inside a <code>/<pre> block goes through this so the model
  // can't inject markup via its code output.
  function escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
  }

  // Expose on window so non-module chat.js can call them.
  window.processMarkdown = processMarkdown;
  window.escapeHtml = escapeHtml;
})();
// webchat — Alpine data component for the chat UI.
//
// This file assumes the host page has already loaded Alpine (window.Alpine)
// via its own bundle. We register the "webchat" component on alpine:init.
// Hosts mount this script AFTER their main bundle (e.g. via two deferred
// <script> tags; deferred scripts run in document order).
//
// Lifecycle:
//   init()           — fetch personas, models, commands, tools
//   newChat()        — show persona/model picker
//   startChat()      — create conversation, persist to sessionStorage
//   send()           — POST /api/chat with SSE stream
//   approveToolCall  — confirm + execute + resubmit
//
// State machine for a turn:
//   draft → sending → streaming → done | tool_calls (waiting on user) | error
//
// Conversations are stored in sessionStorage under "webchat:conversations".
// Per-conversation tool state (enabled list + always-allow list) lives in
// the same record so it survives reloads.

// Register the "webchat" Alpine data component. Must run BEFORE the host's
// Alpine.start() fires alpine:init. We attach a passive alpine:init listener
// unconditionally — it's a no-op if the event already fired (defensive
// against hosts that load Alpine before this script). If Alpine happens to
// already be on window (host loaded us dynamically), register right away.
if (window.Alpine && typeof window.Alpine.data === "function") {
  window.Alpine.data("webchat", webchat);
} else {
  document.addEventListener("alpine:init", () => {
    if (window.Alpine && typeof window.Alpine.data === "function") {
      window.Alpine.data("webchat", webchat);
    }
  });
}

function webchat({ prefix }) {
  let _msgSeq = 0;
  return {
    prefix,
    personas: [{ id: "default", name: "Default" }],
    models: [],
    commands: [],
    allTools: [],
    enabledTools: [],
    showToolsPanel: false,

    conversations: [],
    currentId: null,
    messages: [],
    streaming: false,
    draft: "",
    commandHint: "",

    // Slash command autocomplete dropdown. When draft starts with "/"
    // (and no space yet), we show a filtered list of commands. Arrow
    // up/down navigates, Enter/Tab/click selects, Escape closes.
    slashOpen: false,
    slashIndex: 0,

    // AbortController for the in-flight /api/chat request. Nulled when the
    // turn finishes (cleanly, errored, or cancelled). The Cancel button is
    // visible iff streaming is true.
    abortController: null,

    // Input history (shell-style). inputHistory is shared across all chats
    // and persists to sessionStorage so the user keeps their command memory
    // across browser restarts. historyIndex === -1 means "not browsing";
    // otherwise it's an index into inputHistory. partialDraft captures
    // whatever the user had typed before pressing Up so pressing Down past
    // the most recent entry restores it instead of clearing the field.
    inputHistory: [],
    historyIndex: -1,
    partialDraft: "",

    // Auto-scroll: while streaming we scroll the transcript to the bottom
    // whenever new content arrives. If the user scrolls up (mouse wheel,
    // touch) we stop auto-scrolling so the content they're reading doesn't
    // jump. Scrolling back to the bottom re-enables auto-scroll.
    //
    // We detect "user scrolled up" via wheel/touch events, NOT scroll
    // events — scroll events fire for our own programmatic scrollTop
    // assignments and the timing is racy (the browser fires them as async
    // tasks, which can arrive after our _programmaticScroll flag was reset).
    // Wheel and touchmove are 100% user-initiated — no false positives.
    userHasScrolled: false,

    // Setup modal state
    setupPersonaId: "default",
    setupModel: "",
    setupPersonaSearch: "",
    setupModelSearch: "",
    setupPersonaOpen: false,
    setupPersonaIndex: 0,
    setupModelOpen: false,
    setupModelIndex: 0,
    // Model parameters — populated from the selected persona's [params],
    // user can override any field. null = use API default. Stored on the
    // conversation at startChat() so subsequent turns use the same values.
    setupParams: {
      temperature: null,
      top_p: null,
      top_k: null,
      repeat_penalty: null,
      context_length: null,
      max_tokens: null,
    },

    // "Always allow" set — session-global, shared across all chats in this
    // browser tab. Stored in sessionStorage so it survives page refresh but
    // clears when the browser closes. When the user clicks "Always Allow"
    // for a tool, that tool is auto-approved in every chat for the rest of
    // the session.
    autoAllowTools: [],

    init() {
      Promise.all([
        this.loadPersonas(),
        this.loadModels(),
        this.loadCommands(),
        this.loadTools(),
      ]).then(() => {
        // Auto-start: if the host only offers one persona and one model
        // (e.g. knot's single-tenant system-defined setup), skip the picker
        // and drop the user straight into a chat. They can still hit "New
        // Chat" to come back to the picker if they want.
        if (this.personas.length === 1 && this.models.length === 1 && this.conversations.length === 0) {
          this.selectSetupPersona(this.personas[0]);
          this.setupModel = this.models[0].id;
          this.startChat();
        }
      }).catch((e) => console.warn("webchat init failed", e));

      // Auto-grow the textarea on input, plus toggle slash autocomplete.
      this.$watch("draft", () => {
        this.autosize();
        const matches = this.slashMatches;
        this.slashOpen = matches.length > 0 && !this.draft.includes(" ");
        if (this.slashIndex >= matches.length) this.slashIndex = Math.max(0, matches.length - 1);
      });

      // Restore saved conversations + the chat that was last open so a
      // page refresh picks up where the user left off rather than
      // starting a new chat.
      this.conversations = this.readStorage();
      const savedId = sessionStorage.getItem("webchat:currentId") || "";
      if (savedId && this.conversations.some((c) => c.id === savedId)) {
        this.loadConversation(savedId);
      }

      // Restore shared input history (across all chats, within this session).
      try {
        this.inputHistory = JSON.parse(sessionStorage.getItem("webchat:inputHistory") || "[]");
      } catch { this.inputHistory = []; }

      // Restore session-global auto-allow set (shared across all chats).
      try {
        this.autoAllowTools = JSON.parse(sessionStorage.getItem("webchat:autoAllow") || "[]");
      } catch { this.autoAllowTools = []; }

      // One-time cleanup: remove old localStorage entries left over from
      // before the switch to sessionStorage. Safe no-op once they're gone.
      ["webchat:conversations", "webchat:currentId", "webchat:inputHistory"].forEach((k) => {
        try { localStorage.removeItem(k); } catch {}
      });
    },

    // -- loaders -----------------------------------------------------------
    async loadPersonas() {
      try {
        const r = await fetch(`${this.prefix}/api/personas`, {});
        if (r.ok) {
          const data = await r.json();
          if (Array.isArray(data)) this.personas = data;
        }
      } catch {}
    },
    async loadModels() {
      try {
        const r = await fetch(`${this.prefix}/api/models`, {});
        if (r.ok) {
          const data = await r.json();
          if (Array.isArray(data)) {
            this.models = data;
            if (this.models.length && !this.setupModel) {
              this.setupModel = this.models[0].id;
            }
          }
        }
      } catch {}
    },
    async loadCommands() {
      try {
        const r = await fetch(`${this.prefix}/api/commands`, {});
        if (r.ok) {
          const data = await r.json();
          // Server returns JSON null when no CommandsDir is configured; treat
          // that as "no commands" rather than clobbering our empty default.
          this.commands = Array.isArray(data) ? data : [];
        }
      } catch {}
    },
    async loadTools() {
      try {
        const r = await fetch(`${this.prefix}/api/tools`, {});
        if (r.ok) {
          const data = await r.json();
          if (Array.isArray(data)) {
            this.allTools = data;
            this.enabledTools = this.allTools.map((t) => t.name);
          }
        }
      } catch {}
    },

    // -- persona helpers ---------------------------------------------------
    get currentPersonaName() {
      const c = this.current();
      if (!c) return "";
      const p = this.personas.find((x) => x.id === c.personaId);
      return p ? p.name : "Default";
    },
    get currentModel() {
      const c = this.current();
      return c ? c.model : "";
    },
    get enabledToolsCount() { return this.enabledTools.length; },

    // Reference to the last assistant message (or null). Kept for future
    // use; the template currently uses index comparison (idx ===
    // messages.length - 1) for streaming/dots so it survives proxy identity
    // quirks across rapid pushes.
    get lastAssistant() {
      const last = this.messages[this.messages.length - 1];
      return last && last.role === "assistant" ? last : null;
    },

    // -- conversation persistence -----------------------------------------
    readStorage() {
      try {
        return JSON.parse(sessionStorage.getItem("webchat:conversations") || "[]");
      } catch { return []; }
    },
    writeStorage() {
      // Deep clone with runtime-only fields stripped so a page reload
      // starts clean. showThinking is set to true during streaming so
      // the Thinking disclosure auto-opens while reasoning arrives —
      // but persisting it means the block reopens on refresh, which
      // is wrong. Always start closed on load; auto-open only when
      // NEW reasoning arrives in the current session.
      const cleaned = this.conversations.map((c) => ({
        ...c,
        messages: c.messages.map((m) => {
          const copy = { ...m };
          delete copy.showThinking;
          return copy;
        }),
      }));
      sessionStorage.setItem("webchat:conversations", JSON.stringify(cleaned));
    },
    current() { return this.conversations.find((c) => c.id === this.currentId); },

    // -- new chat setup: searchable persona + model pickers -------------

    get setupPersonaMatches() {
      const s = this.setupPersonaSearch.toLowerCase().trim();
      if (!s) return this.personas;
      return this.personas.filter((p) =>
        p.name.toLowerCase().includes(s) ||
        (p.description || "").toLowerCase().includes(s)
      );
    },

    get setupModelMatches() {
      const s = this.setupModelSearch.toLowerCase().trim();
      if (!s) return this.models;
      return this.models.filter((m) =>
        m.id.toLowerCase().includes(s) ||
        (m.provider || "").toLowerCase().includes(s)
      );
    },

    selectSetupPersona(p) {
      this.setupPersonaId = p.id;
      this.setupPersonaOpen = false;
      this.setupPersonaSearch = "";
      // Auto-set model if persona defines one
      if (p.default_model && this.models.some((m) => m.id === p.default_model)) {
        this.setupModel = p.default_model;
        this.setupModelSearch = "";
      }
      // Populate parameter fields from persona's [params] table
      const pp = p.params || {};
      this.setupParams = {
        temperature: pp.temperature != null ? pp.temperature : null,
        top_p: pp.top_p != null ? pp.top_p : null,
        top_k: pp.top_k != null ? pp.top_k : null,
        repeat_penalty: pp.repeat_penalty != null ? pp.repeat_penalty : null,
        context_length: pp.context_length != null ? pp.context_length : null,
        max_tokens: pp.max_tokens != null ? pp.max_tokens : null,
      };
    },

    selectSetupModel(m) {
      this.setupModel = m.id;
      this.setupModelOpen = false;
      this.setupModelSearch = "";
    },

    onSetupPersonaKeydown(e) {
      if (!this.setupPersonaOpen) return;
      const matches = this.setupPersonaMatches;
      if (e.key === "ArrowDown") { e.preventDefault(); this.setupPersonaIndex = Math.min(matches.length - 1, this.setupPersonaIndex + 1); }
      else if (e.key === "ArrowUp") { e.preventDefault(); this.setupPersonaIndex = Math.max(0, this.setupPersonaIndex - 1); }
      else if (e.key === "Enter") { e.preventDefault(); const m = matches[this.setupPersonaIndex]; if (m) this.selectSetupPersona(m); }
      else if (e.key === "Escape") { this.setupPersonaOpen = false; }
    },

    onSetupModelKeydown(e) {
      if (!this.setupModelOpen) return;
      const matches = this.setupModelMatches;
      if (e.key === "ArrowDown") { e.preventDefault(); this.setupModelIndex = Math.min(matches.length - 1, this.setupModelIndex + 1); }
      else if (e.key === "ArrowUp") { e.preventDefault(); this.setupModelIndex = Math.max(0, this.setupModelIndex - 1); }
      else if (e.key === "Enter") { e.preventDefault(); const m = matches[this.setupModelIndex]; if (m) this.selectSetupModel(m); }
      else if (e.key === "Escape") { this.setupModelOpen = false; }
    },

    // Build the effective params map for the conversation: start with persona
    // defaults, override with user-specified values from the setup fields.
    get setupEffectiveParams() {
      const persona = this.personas.find((p) => p.id === this.setupPersonaId);
      const params = {};
      if (persona && persona.params) {
        for (const [k, v] of Object.entries(persona.params)) {
          params[k] = v;
        }
      }
      for (const [k, v] of Object.entries(this.setupParams)) {
        if (v !== null && v !== "" && v !== undefined) {
          params[k] = typeof v === "string" ? parseFloat(v) : v;
        }
      }
      return params;
    },

    newChat() {
      this.currentId = null;
      this.messages = [];
      localStorage.removeItem("webchat:currentId");
      // Note: autoAllowTools is NOT reset — "Always Allow" is
      // session-global, not per-chat.
    },

    startChat() {
      const id = "c-" + Math.random().toString(36).slice(2, 10);
      const persona = this.personas.find((p) => p.id === this.setupPersonaId) || { id: "default" };
      const conv = {
        id,
        title: "New conversation",
        personaId: persona.id,
        model: this.setupModel,
        params: this.setupEffectiveParams,
        messages: [],
        enabledTools: this.allTools.map((t) => t.name),
        createdAt: Date.now(),
      };
      // Seed the system prompt from the persona
      if (persona.system_prompt) {
        conv.messages.push({ role: "system", content: persona.system_prompt });
      }
      this.conversations.unshift(conv);
      this.currentId = id;
      sessionStorage.setItem("webchat:currentId", id);
      this.messages = conv.messages;
      this.enabledTools = conv.enabledTools;
      this.writeStorage();
      this.$nextTick(() => this.$refs.composer && this.$refs.composer.focus());
    },

    loadConversation(id) {
      const c = this.conversations.find((x) => x.id === id);
      if (!c) return;
      this.currentId = id;
      sessionStorage.setItem("webchat:currentId", id);
      this.messages = c.messages;
      this.enabledTools = c.enabledTools || this.allTools.map((t) => t.name);
      // Loading an existing conversation: park at the bottom and refocus so
      // the user can immediately continue typing.
      this.userHasScrolled = false;
      this.scrollToBottom();
      this.focusComposer();
    },

    deleteCurrent() {
      if (!this.currentId) return;
      this.conversations = this.conversations.filter((c) => c.id !== this.currentId);
      this.writeStorage();
      this.currentId = null;
      sessionStorage.removeItem("webchat:currentId");
      this.messages = [];
    },

    persist() {
      const c = this.current();
      if (c) {
        c.messages = this.messages;
        c.enabledTools = this.enabledTools;
        this.writeStorage();
      }
    },

    // -- send / stream -----------------------------------------------------
    // Composer keydown handler. Plain Enter submits. Shift+Enter inserts a
    // newline (the browser default — we deliberately do NOT use Alpine's
    // `.prevent` modifier on @keydown.enter, because that would also eat
    // Shift+Enter and IME-composition Enter). Enter during IME composition
    // (e.g. CJK input methods) is left alone so the user can confirm the
    // composed character — otherwise the textarea would submit instead of
    // confirming and the placeholder / input state would garble.
    onEnter(e) {
      if (this.slashOpen) {
        e.preventDefault();
        this.selectSlashCommand(this.slashMatches[this.slashIndex]);
        return;
      }
      if (e.isComposing) return;
      if (e.shiftKey) return;
      e.preventDefault();
      this.send();
    },

    // -- input history (shell-style Up/Down) -----------------------------
    onArrowUp(e) {
      if (this.slashOpen) {
        e.preventDefault();
        this.slashIndex = Math.max(0, this.slashIndex - 1);
        return;
      }
      if (!this.shouldNavigateHistory("up", e.target)) return;
      e.preventDefault();
      this.navigateHistory("up");
    },
    onArrowDown(e) {
      if (this.slashOpen) {
        e.preventDefault();
        this.slashIndex = Math.min(this.slashMatches.length - 1, this.slashIndex + 1);
        return;
      }
      if (!this.shouldNavigateHistory("down", e.target)) return;
      e.preventDefault();
      this.navigateHistory("down");
    },

    shouldNavigateHistory(direction, textarea) {
      const { selectionStart, selectionEnd, value } = textarea;
      // Skip when the user has a selection — they're moving the caret, not
      // browsing history.
      if (selectionStart !== selectionEnd) return false;
      const lines = value.split("\n");
      const beforeCursor = value.substring(0, selectionStart);
      const currentLineIndex = beforeCursor.split("\n").length - 1;
      if (direction === "up") {
        // Only navigate when the caret sits on the first line.
        return currentLineIndex === 0;
      }
      // Down: only when on the last line.
      return currentLineIndex === lines.length - 1;
    },

    navigateHistory(direction) {
      if (this.inputHistory.length === 0) return;
      if (direction === "up") {
        if (this.historyIndex === -1) {
          // First Up press: snapshot what they had so Down-past-newest
          // restores it, then jump to the most recent entry.
          this.partialDraft = this.draft;
          this.historyIndex = this.inputHistory.length - 1;
        } else if (this.historyIndex > 0) {
          this.historyIndex--;
        } else {
          // Already at oldest — clamp so we don't wrap.
          return;
        }
        this.draft = this.inputHistory[this.historyIndex];
      } else {
        if (this.historyIndex === -1) return;
        if (this.historyIndex < this.inputHistory.length - 1) {
          this.historyIndex++;
          this.draft = this.inputHistory[this.historyIndex];
        } else {
          // Down past newest: exit history, restore the in-flight draft.
          this.historyIndex = -1;
          this.draft = this.partialDraft;
        }
      }
      // Move caret to the end so a follow-up Up/Down continues from the
      // expected position.
      this.$nextTick(() => {
        const el = this.$refs.composer;
        if (el) {
          el.selectionStart = el.selectionEnd = el.value.length;
          this.autosize();
        }
      });
    },

    async send() {
      const draft = this.draft.trim();
      if (!draft || this.streaming) return;

      // New user turn: re-enable auto-scroll so the transcript follows the
      // forthcoming response. Without this, a user who scrolled up to read
      // earlier output would miss the new turn entirely.
      this.userHasScrolled = false;

      // Record into the shared input history BEFORE clearing the field so
      // Up-arrow recalls the just-sent message. Dedupe + cap so the list
      // stays useful as the user iterates.
      this.recordInputHistory(draft);

      // Slash command?
      if (draft.startsWith("/")) {
        const handled = await this.handleSlash(draft);
        if (handled) {
          this.draft = "";
          return;
        }
      }

      this.messages.push({ id: "msg-" + (++_msgSeq), role: "user", content: draft });
      this.draft = "";
      // Auto-title from first user message
      const c = this.current();
      if (c && c.title === "New conversation") {
        c.title = draft.slice(0, 50);
      }
      this.scrollToBottom();
      this.persist();
      await this.streamTurn();
    },

    // recordInputHistory appends to the shared input history. Consecutive
    // duplicates collapse (the prior occurrence is removed first), and the
    // list is capped at 50 entries so it stays useful after long sessions.
    recordInputHistory(message) {
      const idx = this.inputHistory.indexOf(message);
      if (idx > -1) this.inputHistory.splice(idx, 1);
      this.inputHistory.push(message);
      if (this.inputHistory.length > 50) {
        this.inputHistory = this.inputHistory.slice(-50);
      }
      this.historyIndex = -1;
      this.partialDraft = "";
      try {
        sessionStorage.setItem("webchat:inputHistory", JSON.stringify(this.inputHistory));
      } catch {}
    },

    async streamTurn() {
      this.streaming = true;
      // Push the empty assistant bubble BEFORE issuing the fetch. Knot does
      // this and it's the difference between "model takes 10s to load in
      // LM Studio, user sees a white square with no feedback" and "user
      // sees bouncing dots the instant they hit enter". The dots render
      // inside this empty bubble (gated on !m.content) until the first
      // delta arrives; if the fetch fails we put an error message into
      // this same bubble rather than spawning a new one.
      this.messages.push({
        id: "msg-" + (++_msgSeq), role: "assistant",
        content: "",
        thinking: "",
        showThinking: false,
        tool_calls: [],
      });
      // Scroll immediately so the bouncing dots (inside the empty bubble)
      // are visible while the model loads — not just when the first delta
      // arrives (which can take 10+ seconds on slow providers).
      this.scrollToBottom();
      const reactiveAssistant = this.messages[this.messages.length - 1];
      // Per-turn state for <think>-tag buffering (some open-source models
      // embed reasoning inline in content rather than via reasoning_content).
      let inThinkTag = false;
      let tagBuffer = "";
      // Track open code fences so we can close them at stream end if the
      // model left one dangling — otherwise the markdown renderer would
      // treat the rest of the message (or the next message) as code.
      let openCodeFences = 0;
      try {
        const persona = this.personas.find((p) => p.id === this.current()?.personaId);
        const params = this.current()?.params || (persona && persona.params) || {};
        const tools = this.allTools.filter((t) => this.enabledTools.includes(t.name));

        // AbortController so the Cancel button can interrupt the fetch
        // AND the streaming reader. Lives on `this` so the button can
        // reach it.
        this.abortController = new AbortController();

        const r = await fetch(`${this.prefix}/api/chat`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            model: this.currentModel,
            messages: this.messages.slice(0, -1), // exclude the empty bubble we just pushed
            tools,
            params,
          }),
          signal: this.abortController.signal,
        });
        if (!r.ok) {
          const err = await r.json().catch(() => ({ error: "request failed" }));
          reactiveAssistant.content = "[error] " + (err.error || r.status);
          this.persist();
          return;
        }

        await this.readSSE(r, (ev) => {
          if (ev.type === "reasoning") {
            // Dedicated reasoning channel (OpenAI o1-style via
            // delta.reasoning_content, or hosts that pre-split it).
            reactiveAssistant.thinking += ev.reasoning;
            // Auto-expand the thinking panel as soon as the first chunk
            // arrives so the user sees the model reason in real time. They
            // can collapse it manually after.
            reactiveAssistant.showThinking = true;
            this.scrollToBottom();
          } else if (ev.type === "delta") {
            // Buffer-then-route so <think> tags split across deltas still
            // parse correctly. Same algorithm as knot.
            tagBuffer += ev.delta;
            // Count code fences so we know if one is left open at end.
            openCodeFences += (ev.delta.match(/```/g) || []).length;

            while (tagBuffer) {
              if (inThinkTag) {
                const closeIdx = tagBuffer.indexOf("</think>");
                if (closeIdx === -1) {
                  reactiveAssistant.thinking += tagBuffer;
                  reactiveAssistant.showThinking = true;
                  tagBuffer = "";
                  break;
                }
                reactiveAssistant.thinking += tagBuffer.substring(0, closeIdx);
                reactiveAssistant.showThinking = true;
                tagBuffer = tagBuffer.substring(closeIdx + "</think>".length);
                inThinkTag = false;
              } else {
                const openIdx = tagBuffer.indexOf("<think>");
                if (openIdx === -1) {
                  reactiveAssistant.content += tagBuffer;
                  tagBuffer = "";
                  break;
                }
                reactiveAssistant.content += tagBuffer.substring(0, openIdx);
                tagBuffer = tagBuffer.substring(openIdx + "<think>".length);
                inThinkTag = true;
              }
            }
            this.scrollToBottom();
          } else if (ev.type === "tool_call" && ev.tool_call) {
            // Auto-allow if the user previously chose "Always Allow" for this
            // tool in this conversation. Otherwise mark pending so the
            // approval card renders at the bottom of the bubble.
            const approved = this.autoAllowTools.includes(ev.tool_call.name);
            reactiveAssistant.tool_calls.push({
              id: ev.tool_call.id,
              name: ev.tool_call.name,
              arguments: safeParseArgs(ev.tool_call.arguments),
              approval: approved ? "approved" : "pending",
              auto: approved,
            });
            // Force-scroll: the approval prompt (if pending) lives at the
            // bottom of this bubble, so override userHasScrolled to make
            // sure it's on screen. Without this an open Thinking block
            // above can push the prompt out of view.
            if (!approved) {
              this.userHasScrolled = false;
            }
            this.scrollToBottom();
          } else if (ev.type === "done") {
            // finalize — handled below
          } else if (ev.type === "error") {
            reactiveAssistant.content += `[stream error] ${ev.error}`;
          }
        });

        // Flush any tail still in the tag buffer (model ended mid-tag).
        if (tagBuffer) {
          if (inThinkTag) {
            reactiveAssistant.thinking += tagBuffer;
            reactiveAssistant.showThinking = true;
          } else {
            reactiveAssistant.content += tagBuffer;
          }
          tagBuffer = "";
        }

        // Unclosed code fence at end of stream: append a closing fence so
        // the markdown renderer doesn't paint the rest of the transcript
        // as code. Same fix as knot.
        if (openCodeFences % 2 !== 0) {
          reactiveAssistant.content += "\n```";
        }

        // If the stream ended with only an error (no real content, no tool
        // calls), remove the failed assistant bubble entirely. Leaving it
        // in the messages array would send the error text to the API on
        // the next turn as if it were a real assistant response — which
        // pollutes the conversation and can cause some models to behave
        // oddly. A clean retry should start from the same state as before
        // the failed turn.
        const isErrorResponse = reactiveAssistant.content.startsWith("[stream error]") ||
          reactiveAssistant.content.startsWith("[error]");
        const hasRealContent = reactiveAssistant.content && !isErrorResponse;
        const hasToolCalls = reactiveAssistant.tool_calls.length > 0;

        if (isErrorResponse && !hasToolCalls) {
          // Strip the failed bubble so the conversation is clean for retry.
          const lastIdx = this.messages.length - 1;
          if (lastIdx >= 0 && this.messages[lastIdx] === reactiveAssistant) {
            this.messages.splice(lastIdx, 1);
          }
        }

        // If every tool call was auto-approved (or there are none), continue
        // the turn automatically. Otherwise the inline approval buttons will
        // call approveToolCall/denyToolCall, which execute the tool and then
        // resume the turn.
        const pending = reactiveAssistant.tool_calls.some((c) => c.approval === "pending");
        if (!pending) {
          for (const call of reactiveAssistant.tool_calls) {
            if (call.approval === "approved" && !call.executed) {
              await this.executeToolCall(call);
            }
          }
          if (reactiveAssistant.tool_calls.length > 0) {
            await this.streamTurn();
          }
        }
        this.persist();
      } catch (e) {
        // AbortError is expected when the user hits Cancel — don't surface
        // it as an error, just leave whatever content streamed so far.
        if (e && e.name === "AbortError") {
          this.persist();
        } else {
          reactiveAssistant.content = "[error] " + e.message;
          this.persist();
        }
      } finally {
        this.streaming = false;
        this.abortController = null;
        // Turn is back to the user (or pending tool approval — either way
        // the next legitimate input is the user's). Refocus the composer
        // so they can keep typing without mousing back to the textarea.
        this.focusComposer();
        this.scrollToBottom();
      }
    },

    // cancelStream aborts the in-flight chat request. The fetch's promise
    // rejects with an AbortError which streamTurn catches and treats as a
    // non-error — partial content already streamed stays in place.
    cancelStream() {
      if (this.abortController) {
        this.abortController.abort();
        this.abortController = null;
      }
    },

    async readSSE(response, onEvent) {
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buf = "";
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf("\n\n")) >= 0) {
          const chunk = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          for (const line of chunk.split("\n")) {
            if (!line.startsWith("data:")) continue;
            const payload = line.slice(5).trim();
            if (!payload) continue;
            try { onEvent(JSON.parse(payload)); } catch {}
          }
        }
      }
    },

    // -- tool-call confirmation -------------------------------------------
    // approveToolCall fires when the user clicks Allow or Always Allow on
    // an inline tool-call card. `always` records the tool in the session's
    // auto-allow set so future calls to the same tool skip the prompt.
    async approveToolCall(call, always) {
      if (call.approval !== "pending") return;
      if (always && !this.autoAllowTools.includes(call.name)) {
        this.autoAllowTools.push(call.name);
        sessionStorage.setItem("webchat:autoAllow", JSON.stringify(this.autoAllowTools));
      }
      call.approval = "approving";
      await this.executeToolCall(call);
      await this.maybeResumeAfterApproval();
    },

    async denyToolCall(call) {
      if (call.approval !== "pending") return;
      call.approval = "denying";
      call.result = "[denied by user]";
      // Append a tool message so the model knows the user declined.
      this.messages.push({
        id: "msg-" + (++_msgSeq), role: "tool",
        tool_call_id: call.id,
        tool_name: call.name,
        content: "[denied by user]",
      });
      call.approval = "denied";
      this.persist();
      this.scrollToBottom();
      await this.maybeResumeAfterApproval();
    },

    // maybeResumeAfterApproval checks whether the most recent assistant
    // message has any tool calls still awaiting user action. If not, the
    // turn continues: reset the auto-scroll flag (so the follow-up is
    // visible even if the user scrolled while reading the approval card)
    // and stream the next assistant response.
    //
    // IMPORTANT: we search BACKWARDS through messages for the last
    // assistant message — by this point tool-result messages have been
    // appended after it, so checking messages[length-1] would miss it.
    // Without this lookup the model never sees the tool result and stays
    // silent after the user clicks Allow.
    async maybeResumeAfterApproval() {
      let lastAssistant = null;
      for (let i = this.messages.length - 1; i >= 0; i--) {
        if (this.messages[i].role === "assistant") {
          lastAssistant = this.messages[i];
          break;
        }
      }
      if (!lastAssistant) return;
      const stillPending = lastAssistant.tool_calls.some(
        (c) => c.approval === "pending" || c.approval === "approving" || c.approval === "denying"
      );
      if (stillPending) return;
      this.userHasScrolled = false;
      await this.streamTurn();
    },

    // executeToolCall runs the tool via /api/tools/call and appends a tool
    // message recording the result (for the model's context next turn).
    // Also stashes the result on the call object as `call.result` so the
    // template can render the call and its output together inside the
    // collapsible "Tool calls" block — without that, the result would
    // float as a separate tool-role bubble below the assistant message
    // and lose its association with the call that produced it.
    // Idempotent via the call.executed flag so accidental double-clicks
    // don't double-execute.
    async executeToolCall(call) {
      if (call.executed) return;
      call.executed = true;
      try {
        const r = await fetch(`${this.prefix}/api/tools/call`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ name: call.name, arguments: call.arguments }),
        });
        const data = await r.json();
        const content = data.content != null ? data.content : JSON.stringify(data);
        call.result = content;
        call.isError = !!data.is_error;
        this.messages.push({
          id: "msg-" + (++_msgSeq), role: "tool",
          tool_call_id: call.id,
          tool_name: call.name,
          content,
        });
        call.approval = call.isError ? "error" : "approved";
      } catch (e) {
        call.result = "[error] " + e.message;
        call.isError = true;
        this.messages.push({
          id: "msg-" + (++_msgSeq), role: "tool",
          tool_call_id: call.id,
          tool_name: call.name,
          content: "[error] " + e.message,
        });
        call.approval = "error";
      }
      this.persist();
      this.scrollToBottom();
    },

    // -- slash commands ----------------------------------------------------
    // Returns the list of commands matching what the user has typed so far
    // after the leading "/". Once a space appears (the user is typing
    // arguments), the dropdown closes.
    get slashMatches() {
      if (!this.draft.startsWith("/")) return [];
      const name = this.draft.slice(1).split(/\s/)[0].toLowerCase();
      if (!name) return (this.commands || []).slice();
      return (this.commands || []).filter((c) => c.name.startsWith(name));
    },

    get commandHint() {
      if (!this.slashOpen) return "";
      const matches = this.slashMatches;
      if (matches.length === 0) return "no matching command";
      return `${matches.length} command${matches.length === 1 ? "" : "s"}`;
    },

    selectSlashCommand(cmd) {
      if (!cmd) return;
      this.slashOpen = false;
      // If the command body doesn't use $ARGUMENTS, it takes no arguments —
      // submit immediately rather than leaving the user with a pending draft
      // and an extra Enter press.
      if (!cmd.body || !cmd.body.includes("$ARGUMENTS")) {
        this.draft = "/" + cmd.name;
        this.send();
        return;
      }
      // Command expects arguments — fill the name and let the user type.
      this.draft = "/" + cmd.name + " ";
      this.$nextTick(() => this.$refs.composer && this.$refs.composer.focus());
    },

    async handleSlash(input) {
      const trimmed = input.trim();
      const sp = trimmed.indexOf(" ");
      const name = (sp === -1 ? trimmed.slice(1) : trimmed.slice(1, sp)).toLowerCase();
      const args = sp === -1 ? "" : trimmed.slice(sp + 1).trim();
      const cmd = (this.commands || []).find((c) => c.name === name);
      if (!cmd) return false;
      // If the command declares allowed-tools in frontmatter, add them
      // to the session-global auto-allow set so the model can call them
      // without prompting. Format: comma-separated tool names, optionally
      // with Claude-style patterns like Bash(git:*) — we strip the pattern
      // for now and match on tool name only.
      if (cmd.allowed_tools) {
        for (const raw of cmd.allowed_tools.split(",")) {
          const toolName = raw.trim().split("(")[0].trim();
          if (toolName && !this.autoAllowTools.includes(toolName)) {
            this.autoAllowTools.push(toolName);
          }
        }
        sessionStorage.setItem("webchat:autoAllow", JSON.stringify(this.autoAllowTools));
      }
      const rendered = (cmd.body || "").replaceAll("$ARGUMENTS", args);
      this.messages.push({ id: "msg-" + (++_msgSeq), role: "user", content: rendered });
      this.scrollToBottom();
      this.persist();
      await this.streamTurn();
      return true;
    },

    // -- UI helpers --------------------------------------------------------
    // formatContent renders model output (markdown) to HTML. Defined here
    // rather than inline in the template so the template can call
    // m.content via formatContent(m.content) and get code blocks, tables,
    // lists, etc. Falls back to the raw text if markdown.js isn't loaded.
    formatContent(content) {
      if (!content) return "";
      if (typeof window.processMarkdown === "function") {
        return window.processMarkdown(content);
      }
      return content;
    },

    // scrollToBottom pins the transcript to the latest content. No-op when
    // the user has scrolled up to read earlier output — knot's pattern.
    // Always called via $nextTick so the DOM has the new content first.
    scrollToBottom() {
      if (this.userHasScrolled) return;
      this.$nextTick(() => {
        const el = this.$refs.messages;
        if (el) el.scrollTop = el.scrollHeight;
      });
    },

    // onWheel detects the user scrolling UP (deltaY < 0) and disables
    // auto-scroll. Scrolling DOWN re-enables it when they reach the bottom.
    // This replaces the old onMessagesScroll approach which was racy —
    // scroll events from our own scrollTop assignments could arrive after
    // the _programmaticScroll flag was reset, causing false "user scrolled
    // up" detections that killed auto-scroll mid-stream.
    onWheel(e) {
      if (e.deltaY < 0) {
        this.userHasScrolled = true;
      } else {
        const el = this.$refs.messages;
        if (el) {
          const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
          if (atBottom) this.userHasScrolled = false;
        }
      }
    },

    // focusComposer puts keyboard focus back on the textarea. Called at the
    // end of every assistant turn so the user can immediately start typing
    // their next message without having to click.
    focusComposer() {
      this.$nextTick(() => {
        const el = this.$refs.composer;
        if (el) el.focus();
      });
    },

    autosize() {
      const el = this.$refs.composer;
      if (!el) return;
      el.style.height = "auto";
      el.style.height = Math.min(el.scrollHeight, 160) + "px";
    },
    toggleAllTools(ev) {
      if (ev.target.checked) {
        this.enabledTools = this.allTools.map((t) => t.name);
      } else {
        this.enabledTools = [];
      }
    },
  };
}

// safeParseArgs normalises the tool-call arguments payload into a plain
// object. The server emits arguments as json.RawMessage, which Go re-
// marshals to whatever JSON value the raw bytes encode — usually an
// object, occasionally an array, sometimes a primitive. JSON.parse only
// accepts strings, so we have to detect what we got:
//   - already-parsed object/array → return as-is
//   - JSON string → parse it
//   - anything else → wrap under _raw so the UI has *something* to render
//     (better than the old `String(obj)` which produced "[object Object]").
function safeParseArgs(raw) {
  if (raw == null) return {};
  if (typeof raw === "object") return raw;
  if (typeof raw !== "string") return { _raw: String(raw) };
  const trimmed = raw.trim();
  if (!trimmed) return {};
  try { return JSON.parse(trimmed); } catch { return { _raw: trimmed }; }
}
