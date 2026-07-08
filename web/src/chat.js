// lmchatkit — markdown processor.
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
// can call them without ES imports. This keeps lmchatkit's bundle friendly
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
// lmchatkit — Alpine data component for the chat UI.
//
// This file assumes the host page has already loaded Alpine (window.Alpine)
// via its own bundle. We register the "lmchatkit" component on alpine:init.
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
// Conversations are stored in sessionStorage under "lmchatkit:conversations".
// Per-conversation tool state (enabled list + always-allow list) lives in
// the same record so it survives reloads.

// Register the "lmchatkit" Alpine data component. Must run BEFORE the host's
// Alpine.start() fires alpine:init. We attach a passive alpine:init listener
// unconditionally — it's a no-op if the event already fired (defensive
// against hosts that load Alpine before this script). If Alpine happens to
// already be on window (host loaded us dynamically), register right away.
if (window.Alpine && typeof window.Alpine.data === "function") {
  window.Alpine.data("lmchatkit", lmchatkit);
} else {
  document.addEventListener("alpine:init", () => {
    if (window.Alpine && typeof window.Alpine.data === "function") {
      window.Alpine.data("lmchatkit", lmchatkit);
    }
  });
}

function lmchatkit({ prefix, browserOnly = false }) {
  let _msgSeq = 0;
  return {
    prefix,
    personas: [{ id: "default", name: "Default" }],
    models: [],
    commands: [],

    conversations: [],
    currentId: null,
    messages: [],
    streaming: false,
    draft: "",

    // Slash command autocomplete dropdown. When draft starts with "/"
    // (and no space yet), we show a filtered list of commands. Arrow
    // up/down navigates, Enter/Tab/click selects, Escape closes.
    slashOpen: false,
    slashIndex: 0,

    // MCP prompts + resources (loaded from API on init).
    prompts: [],
    resources: [],

    // /prompt autocomplete: when draft starts with "/prompt " show a
    // dropdown of available MCP prompts.
    promptMenuOpen: false,
    promptMenuIndex: 0,

    // @resource autocomplete: when draft ends with "@<partial>" show a
    // dropdown of available resources.
    resourceMenuOpen: false,
    resourceMenuIndex: 0,

    // AbortController for the in-flight /api/chat request. Nulled when the
    // turn finishes (cleanly, errored, or cancelled). The Cancel button is
    // visible iff this is non-null AND streaming is true.
    abortController: null,

    // Which message index was just copied to clipboard (-1 = none).
    // Shows a checkmark on the copied message for 2 seconds.
    copiedIdx: -1,
    copiedHtmlIdx: -1,

    // Delete confirmation modal state.
    showDeleteChatModal: false,
    deleteChatTarget: null,

    // Inline rename state for the sidebar. renamingId is the conversation
    // being edited; renameDraft holds the current text. When non-null,
    // that conversation item renders an <input> instead of a <button>.
    renamingId: null,
    renameDraft: "",

    // Server-side history mode. Detected on init by probing
    // GET /api/conversations. When true, conversations are loaded/saved
    // via the server API and SSE events keep multiple tabs in sync.
    // When false, sessionStorage is used (current behaviour).
    _serverMode: false,
    _lastSavedId: null,
    _lastSavedAt: 0,

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

    // Edit-conversation modal state. Opened from the sidebar pencil button;
    // mirrors the new-chat setup screen so an existing chat's persona, model
    // and params can all be changed without starting over. Replaces the older
    // inline rename flow, which is kept below for hosts that still use it in
    // their own templates.
    showEditModal: false,
    editConvId: null,
    editTitle: "",
    editModel: "",
    editModelSearch: "",
    editModelOpen: false,
    editModelIndex: 0,
    editPersonaId: "default",
    editPersonaSearch: "",
    editPersonaOpen: false,
    editPersonaIndex: 0,
    editParams: {
      temperature: null,
      top_p: null,
      top_k: null,
      repeat_penalty: null,
      context_length: null,
      max_tokens: null,
    },
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

    // --- Sidebar toggle (conversation history list) ---
    // Hosts that render a collapsible sidebar (e.g. knot's floating window)
    // bind this to a toggle button. Full-page hosts can leave it true.
    showSidebar: false,

    // --- Floating-window state (optional) ---
    // When a host renders the chat as a floating, draggable, resizable
    // window (e.g. knot's floating panel), these properties track position,
    // size, and drag/resize interactions. They persist to localStorage so
    // the window opens where the user last left it. Hosts that render the
    // chat as a full page (e.g. llmrouter) simply ignore these — the
    // bindings are only active when the host template wires them up.
    winPos: { x: null, y: null },
    winSize: { w: null, h: null },
    winMaximized: false,
    _dragging: false,
    _dragOffset: { x: 0, y: 0 },
    _resizing: null, // null | "se" | "sw" | "ne" | "nw" | "e" | "w" | "n" | "s"
    _resizeStart: null,

    init() {
      // Detect server-side history mode first — determines whether
      // conversations live on the server (persistent, cross-tab) or in
      // sessionStorage (ephemeral, per-tab).
      this.detectServerMode().then(() => {
        return Promise.all([
          this.loadPersonas(),
          this.loadModels(),
          this.loadCommands(),
          this.loadPrompts(),
          this.loadResources(),
        ]);
      }).then(() => {
        if (this.personas.length === 1 && this.models.length === 1 && this.conversations.length === 0) {
          this.selectSetupPersona(this.personas[0]);
          this.setupModel = this.models[0].id;
          this.startChat();
        }
      }).catch((e) => console.warn("lmchatkit init failed", e));

      this.$watch("draft", () => {
        this.autosize();
        const matches = this.slashMatches;
        this.slashOpen = matches.length > 0 && !this.draft.includes(" ");
        if (this.slashIndex >= matches.length) this.slashIndex = Math.max(0, matches.length - 1);
        const pm = this.promptMenuMatches;
        this.promptMenuOpen = pm.length > 0;
        if (this.promptMenuIndex >= pm.length) this.promptMenuIndex = 0;
        const rm = this.resourceMenuMatches;
        this.resourceMenuOpen = rm.length > 0;
        if (this.resourceMenuIndex >= rm.length) this.resourceMenuIndex = 0;
      });

      try {
        this.inputHistory = JSON.parse(sessionStorage.getItem("lmchatkit:inputHistory") || "[]");
      } catch { this.inputHistory = []; }

      try {
        this.autoAllowTools = JSON.parse(sessionStorage.getItem("lmchatkit:autoAllow") || "[]");
      } catch { this.autoAllowTools = []; }

      // Always auto-approve the virtual skill-retrieval tool — it's a
      // read-only context fetch, not a user-visible action. The model
      // calls it to load skill instructions; the user never needs to
      // approve it.
      if (!this.autoAllowTools.includes("lmchatkit__get_skill")) {
        this.autoAllowTools.push("lmchatkit__get_skill");
      }

      // Restore saved floating-window position/size (if any).
      try {
        const saved = JSON.parse(localStorage.getItem("lmchatkit:winGeo") || "{}");
        if (saved.pos) this.winPos = { ...this.winPos, ...saved.pos };
        if (saved.size) this.winSize = { ...this.winSize, ...saved.size };
        if (saved.maximized) this.winMaximized = saved.maximized;
      } catch {}

      // When the browser window is resized, forget the chat window's
      // remembered geometry so it can never end up off-screen. Resetting
      // to null lets winStyle() recompute a sensible default against the
      // current viewport (the window tracks the bottom-right corner as
      // the viewport changes).
      window.addEventListener("resize", () => {
        this.winPos = { x: null, y: null };
        this.winSize = { w: null, h: null };
        this.winMaximized = false;
        try { localStorage.removeItem("lmchatkit:winGeo"); } catch {}
      });
    },

    // --- Floating-window geometry helpers ---
    // These are only active when the host template binds them (e.g.
    // @mousedown="startDrag($event)" on the window header). On a full-page
    // host like llmrouter they are never called.

    _saveWinGeo() {
      try {
        localStorage.setItem("lmchatkit:winGeo", JSON.stringify({
          pos: this.winPos,
          size: this.winSize,
          maximized: this.winMaximized,
        }));
      } catch {}
    },

    winStyle() {
      if (this.winMaximized) {
        return "left:0; top:0; right:0; bottom:0; width:100vw; height:100vh;";
      }
      // Fall back to a sensible bottom-right default (sized to the current
      // viewport) when no position/size has been set or remembered. This
      // keeps the window on-screen after geometry is forgotten on a
      // browser resize instead of collapsing to the top-left corner.
      const w = this.winSize.w != null ? this.winSize.w : Math.min(480, Math.max(320, window.innerWidth - 40));
      const h = this.winSize.h != null ? this.winSize.h : Math.min(560, Math.max(240, window.innerHeight - 40));
      const x = this.winPos.x != null ? this.winPos.x : Math.max(20, window.innerWidth - w - 20);
      const y = this.winPos.y != null ? this.winPos.y : Math.max(20, window.innerHeight - h - 20);
      return `left:${x}px; top:${y}px; width:${w}px; height:${h}px;`;
    },

    startDrag(e) {
      if (this.winMaximized) return;
      // Only start on left mouse button, and not when clicking interactive
      // elements inside the header (buttons, inputs).
      if (e.button !== 0) return;
      const target = e.target.closest("button, input, select, a, [data-no-drag]");
      if (target) return;
      e.preventDefault();
      const panel = e.currentTarget.closest("[data-chat-window]");
      if (!panel) return;
      const rect = panel.getBoundingClientRect();
      this._dragging = true;
      this._dragOffset = { x: e.clientX - rect.left, y: e.clientY - rect.top };
      this._dragW = rect.width;
      this._dragH = rect.height;
      document.body.style.userSelect = "none";
      const move = (ev) => this.onDrag(ev);
      const up = () => {
        this._dragging = false;
        document.body.style.userSelect = "";
        document.removeEventListener("mousemove", move);
        document.removeEventListener("mouseup", up);
        this._saveWinGeo();
      };
      document.addEventListener("mousemove", move);
      document.addEventListener("mouseup", up);
    },

    onDrag(e) {
      if (!this._dragging) return;
      const x = e.clientX - this._dragOffset.x;
      const y = e.clientY - this._dragOffset.y;
      // Keep the whole window inside the viewport — it can't be dragged
      // off the right/bottom edge (nor off the top/left).
      const w = this._dragW || this.winSize.w || 400;
      const h = this._dragH || this.winSize.h || 300;
      const maxX = Math.max(0, window.innerWidth - w);
      const maxY = Math.max(0, window.innerHeight - h);
      this.winPos = {
        x: Math.max(0, Math.min(x, maxX)),
        y: Math.max(0, Math.min(y, maxY)),
      };
    },

    startResize(e, dir) {
      if (this.winMaximized) return;
      if (e.button !== 0) return;
      e.preventDefault();
      e.stopPropagation();
      const panel = e.currentTarget.closest("[data-chat-window]");
      if (!panel) return;
      const rect = panel.getBoundingClientRect();
      this._resizing = dir;
      this._resizeStart = {
        mx: e.clientX,
        my: e.clientY,
        x: rect.left,
        y: rect.top,
        w: rect.width,
        h: rect.height,
      };
      document.body.style.userSelect = "none";
      const move = (ev) => this.onResize(ev);
      const up = () => {
        this._resizing = null;
        document.body.style.userSelect = "";
        document.removeEventListener("mousemove", move);
        document.removeEventListener("mouseup", up);
        this._saveWinGeo();
      };
      document.addEventListener("mousemove", move);
      document.addEventListener("mouseup", up);
    },

    onResize(e) {
      if (!this._resizing || !this._resizeStart) return;
      const s = this._resizeStart;
      const dx = e.clientX - s.mx;
      const dy = e.clientY - s.my;
      const dir = this._resizing;
      const minW = 320, minH = 240;

      let w = s.w, h = s.h, x = s.x, y = s.y;
      // Constrain each edge to the viewport so the window can't be
      // resized off-screen on any side.
      if (dir.includes("e")) w = Math.max(minW, Math.min(window.innerWidth - s.x, s.w + dx));
      if (dir.includes("s")) h = Math.max(minH, Math.min(window.innerHeight - s.y, s.h + dy));
      if (dir.includes("w")) { w = Math.max(minW, Math.min(s.x + s.w, s.w - dx)); x = s.x + (s.w - w); }
      if (dir.includes("n")) { h = Math.max(minH, Math.min(s.y + s.h, s.h - dy)); y = s.y + (s.h - h); }

      this.winSize = { w, h };
      this.winPos = { x, y };
    },

    toggleMaximize() {
      this.winMaximized = !this.winMaximized;
      this._saveWinGeo();
    },

    _browserOnly: browserOnly,

    async detectServerMode() {
      if (this._browserOnly) {
        this._serverMode = false;
        this.conversations = this.readStorage();
        const savedId = sessionStorage.getItem("lmchatkit:currentId") || "";
        if (savedId && this.conversations.some((c) => c.id === savedId)) {
          this.loadConversation(savedId);
        }
        this.subscribeToEvents();
        return;
      }
      try {
        const r = await fetch(`${this.prefix}/api/conversations`);
        if (r.ok) {
          this._serverMode = true;
          const list = await r.json();
          this.conversations = Array.isArray(list) ? list : [];
          this.subscribeToEvents();
          const savedId = sessionStorage.getItem("lmchatkit:currentId") || "";
          if (savedId && this.conversations.some((c) => c.id === savedId)) {
            await this.loadConversation(savedId);
          }
          return;
        }
      } catch {}
      this._serverMode = false;
      this.conversations = this.readStorage();
      const savedId = sessionStorage.getItem("lmchatkit:currentId") || "";
      if (savedId && this.conversations.some((c) => c.id === savedId)) {
        this.loadConversation(savedId);
      }
    },

    // hasPendingToolApprovals returns true if any assistant message has
    // tool calls awaiting user action. Used to suppress SSE-triggered
    // reloads that would clobber browser-only approval state.
    hasPendingToolApprovals() {
      return this.messages.some((m) =>
        m.tool_calls && m.tool_calls.some((c) =>
          c.approval === "pending" || c.approval === "approving" || c.approval === "denying"
        )
      );
    },

    subscribeToEvents() {
      const es = new EventSource(`${this.prefix}/api/events`);
      es.onmessage = (e) => {
        try {
          const event = JSON.parse(e.data);
          switch (event.type) {
            case "conversation_saved":
              // Skip our own saves — we already have the data locally.
              if (event.id === this.currentId && this._lastSavedId === event.id && Date.now() - this._lastSavedAt < 2000) {
                this._lastSavedId = null;
                break;
              }

              if (event.id === this.currentId) {
                // Another tab updated the conversation we're currently
                // viewing. Reload the messages so new content appears
                // live — but NOT if we're mid-stream (would clobber the
                // in-progress assistant bubble) or if there are pending
                // tool call approvals (the server data doesn't carry
                // browser-only approval state, so reloading would
                // silently drop the approval UI and deadlock the chat).
                if (!this.streaming && !this.hasPendingToolApprovals()) {
                  this.reloadCurrentConversation();
                }
                // Refresh sidebar for updated title/timestamp.
                this.loadConversationList();
              } else {
                // A different conversation was updated. Refresh the
                // sidebar and mark it unread so the user knows to
                // check it.
                this.loadConversationList().then(() => {
                  const conv = this.conversations.find((c) => c.id === event.id);
                  if (conv) conv.unread = true;
                });
              }
              break;
            case "conversation_deleted":
              this.loadConversationList();
              if (event.id === this.currentId) {
                this.newChat();
              }
              break;
            case "conversation_renamed":
              // Another tab renamed a conversation. Refresh the sidebar
              // to show the new title. No unread marking — a title change
              // is not new content.
              this.loadConversationList();
              break;
            case "prompts_changed": this.loadPrompts(); break;
            case "resources_changed": this.loadResources(); break;
            case "personas_changed": this.loadPersonas(); break;
            case "commands_changed": this.loadCommands(); break;
          }
        } catch {}
      };
    },

    async loadConversationList() {
      try {
        const r = await fetch(`${this.prefix}/api/conversations`);
        if (r.ok) {
          const list = await r.json();
          if (Array.isArray(list)) {
            // Preserve unread flags from existing conversations so they
            // survive list refreshes. Without this, every call to
            // loadConversationList would wipe all unread indicators.
            const prevUnread = new Set(
              this.conversations.filter((c) => c.unread).map((c) => c.id)
            );
            this.conversations = list.map((c) => ({
              ...c,
              unread: prevUnread.has(c.id),
            }));
          }
        }
      } catch {}
    },

    // normalizeMessages ensures every message loaded from the server has
    // the fields the UI needs: a unique id (Alpine x-for :key — without it
    // duplicate undefined keys cause only the last message to render) and
    // a tool_calls array (so .push/.filter don't crash on undefined).
    // System messages are stripped — the server derives the system prompt
    // from the persona on each /api/chat request.
    normalizeMessages(msgs) {
      const out = (msgs || [])
        .filter((m) => m.role !== "system")
        .map((m) => ({
          ...m,
          id: m.id || ("msg-" + (++_msgSeq)),
          tool_calls: m.tool_calls || [],
        }));
      // Advance _msgSeq past any "msg-N" ids carried over from storage so the
      // fresh ids minted below (and the next locally-created message) can't
      // collide with a kept one. Without this, a page reload resets the
      // sequence to 0 and the next send produces msg-1 again — colliding with
      // the first loaded message. Alpine's x-for dedupes by :key, so the
      // earlier message stops rendering even though it stays in this.messages
      // (and is still sent to the model).
      for (const m of out) {
        const n = typeof m.id === "string" && m.id.startsWith("msg-")
          ? parseInt(m.id.slice(4), 10) : NaN;
        if (!Number.isNaN(n) && n >= _msgSeq) _msgSeq = n;
      }
      // De-duplicate: a conversation saved while the sequencing bug above was
      // active can carry duplicate "msg-N" ids on disk (e.g. two msg-1). On
      // load that makes Alpine drop all but one colliding message from the
      // rendered transcript. Give any collided id a fresh, guaranteed-unique
      // sequence number so the full history renders again. This also repairs
      // the stored conversation the next time it's persisted.
      const seen = new Set();
      for (const m of out) {
        if (seen.has(m.id)) {
          m.id = "msg-" + (++_msgSeq);
        }
        seen.add(m.id);
      }
      return out;
    },

    // reloadCurrentConversation fetches the conversation we're currently
    // viewing and replaces messages in-place. Used when another tab saves
    // the same conversation — the new content appears live. Unlike
    // loadConversation, this does NOT reset currentId, sessionStorage,
    // or steal focus — those are already correct. It also
    // does NOT force userHasScrolled=false, so if the user scrolled up
    // to read, they stay where they are.
    //
    // Race guard: if the user sends a message while the fetch is in
    // flight (changing this.messages.length), the reload is aborted so
    // we don't clobber their just-pushed message.
    async reloadCurrentConversation() {
      if (!this.currentId) return;
      const prevLen = this.messages.length;
      try {
        const r = await fetch(`${this.prefix}/api/conversations/${this.currentId}`);
        if (!r.ok) return;
        if (this.messages.length !== prevLen) return;
        const data = await r.json();
        if (this.messages.length !== prevLen) return;

        // Preserve browser-only tool call state (result, approval,
        // executed, isError, auto) that the server doesn't persist.
        // Without this, an SSE-triggered reload after a deny/execute
        // would wipe the result from the UI.
        const prevTCState = new Map();
        for (const m of this.messages) {
          if (m.role === "assistant" && m.tool_calls) {
            for (const tc of m.tool_calls) {
              prevTCState.set(tc.id, {
                result: tc.result,
                approval: tc.approval,
                executed: tc.executed,
                isError: tc.isError,
                auto: tc.auto,
              });
            }
          }
        }

        this.messages = this.normalizeMessages(data.messages);

        // Merge back browser-only state onto the reloaded tool calls.
        for (const m of this.messages) {
          if (m.role === "assistant" && m.tool_calls) {
            for (const tc of m.tool_calls) {
              const prev = prevTCState.get(tc.id);
              if (prev) {
                tc.result = prev.result;
                tc.approval = prev.approval;
                tc.executed = prev.executed;
                tc.isError = prev.isError;
                tc.auto = prev.auto;
              }
            }
          }
        }

        this.scrollToBottom();
      } catch {}
    },

    // -- loaders -----------------------------------------------------------
    async loadPersonas() {
      try {
        const r = await fetch(`${this.prefix}/api/personas`, { headers: this._etagHeaders("personas") });
        if (r.status === 304) return;
        if (r.ok) {
          this._storeETag("personas", r);
          const data = await r.json();
          if (Array.isArray(data)) this.personas = data;
        }
      } catch {}
    },
    async loadModels() {
      try {
        const r = await fetch(`${this.prefix}/api/models`, { headers: this._etagHeaders("models") });
        if (r.status === 304) return;
        if (r.ok) {
          this._storeETag("models", r);
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
        const r = await fetch(`${this.prefix}/api/commands`, { headers: this._etagHeaders("commands") });
        if (r.status === 304) return;
        if (r.ok) {
          this._storeETag("commands", r);
          const data = await r.json();
          this.commands = Array.isArray(data) ? data : [];
        }
      } catch {}
    },
    async loadPrompts() {
      try {
        const r = await fetch(`${this.prefix}/api/prompts`, { headers: this._etagHeaders("prompts") });
        if (r.status === 304) return;
        if (r.ok) {
          this._storeETag("prompts", r);
          const data = await r.json();
          this.prompts = Array.isArray(data) ? data : [];
        }
      } catch {}
    },
    async loadResources() {
      try {
        const r = await fetch(`${this.prefix}/api/resources`, { headers: this._etagHeaders("resources") });
        if (r.status === 304) return;
        if (r.ok) {
          this._storeETag("resources", r);
          const data = await r.json();
          this.resources = Array.isArray(data) ? data : [];
        }
      } catch {}
    },

    // ETag helpers: store the server's ETag per endpoint and send it back
    // as If-None-Match on subsequent requests. On 304 the data hasn't
    // changed — skip parsing entirely.
    _etags: {},
    _etagHeaders(key) {
      const etag = this._etags[key];
      return etag ? { "If-None-Match": etag } : {};
    },
    _storeETag(key, resp) {
      const etag = resp.headers.get("ETag");
      if (etag) this._etags[key] = etag;
    },

    // -- persona helpers ---------------------------------------------------
    get currentPersonaName() {
      const c = this.current;
      if (!c) return "";
      const p = this.personas.find((x) => x.id === (c.persona_id || c.personaId));
      return p ? p.name : "Default";
    },
    get currentModel() {
      const c = this.current;
      return c ? c.model : "";
    },


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
        return JSON.parse(sessionStorage.getItem("lmchatkit:conversations") || "[]");
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
      sessionStorage.setItem("lmchatkit:conversations", JSON.stringify(cleaned));
    },
    get current() { return this.conversations.find((c) => c.id === this.currentId); },

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

    // -- edit conversation modal (title + persona + model + params) -------
    // Opened from the sidebar pencil button. Mirrors the new-chat setup
    // screen so any metadata of an existing chat can be changed without
    // starting over. Saving PATCHes {title, persona_id, model, params}; if
    // the edited conversation is the one currently open, its fields update
    // live (currentModel / currentPersonaName read through current()) so the
    // next send uses them.

    openEdit(c) {
      this.editConvId = c.id;
      this.editTitle = c.title || "";
      this.editModel = c.model || "";
      this.editPersonaId = c.persona_id || "default";
      this.editModelSearch = "";
      this.editModelOpen = false;
      this.editModelIndex = 0;
      this.editPersonaSearch = "";
      this.editPersonaOpen = false;
      this.editPersonaIndex = 0;
      // Seed param fields with the conversation's current params. Missing
      // keys stay null (= "use API default"), matching the setup screen.
      const cp = c.params || {};
      this.editParams = {
        temperature: cp.temperature != null ? cp.temperature : null,
        top_p: cp.top_p != null ? cp.top_p : null,
        top_k: cp.top_k != null ? cp.top_k : null,
        repeat_penalty: cp.repeat_penalty != null ? cp.repeat_penalty : null,
        context_length: cp.context_length != null ? cp.context_length : null,
        max_tokens: cp.max_tokens != null ? cp.max_tokens : null,
      };
      this.showEditModal = true;
    },

    get editPersonaMatches() {
      const s = this.editPersonaSearch.toLowerCase().trim();
      if (!s) return this.personas;
      return this.personas.filter((p) =>
        p.name.toLowerCase().includes(s) ||
        (p.description || "").toLowerCase().includes(s)
      );
    },

    get editModelMatches() {
      const s = this.editModelSearch.toLowerCase().trim();
      if (!s) return this.models;
      return this.models.filter((m) =>
        m.id.toLowerCase().includes(s) ||
        (m.provider || "").toLowerCase().includes(s)
      );
    },

    selectEditPersona(p) {
      this.editPersonaId = p.id;
      this.editPersonaOpen = false;
      this.editPersonaSearch = "";
      // Same behaviour as the setup screen: adopt the persona's default
      // model (if any) and seed the param fields from its [params] table,
      // so editing mirrors creating a conversation against that persona.
      if (p.default_model && this.models.some((m) => m.id === p.default_model)) {
        this.editModel = p.default_model;
        this.editModelSearch = "";
      }
      const pp = p.params || {};
      this.editParams = {
        temperature: pp.temperature != null ? pp.temperature : null,
        top_p: pp.top_p != null ? pp.top_p : null,
        top_k: pp.top_k != null ? pp.top_k : null,
        repeat_penalty: pp.repeat_penalty != null ? pp.repeat_penalty : null,
        context_length: pp.context_length != null ? pp.context_length : null,
        max_tokens: pp.max_tokens != null ? pp.max_tokens : null,
      };
    },

    selectEditModel(m) {
      this.editModel = m.id;
      this.editModelOpen = false;
      this.editModelSearch = "";
    },

    onEditPersonaKeydown(e) {
      if (!this.editPersonaOpen) return;
      const matches = this.editPersonaMatches;
      if (e.key === "ArrowDown") { e.preventDefault(); this.editPersonaIndex = Math.min(matches.length - 1, this.editPersonaIndex + 1); }
      else if (e.key === "ArrowUp") { e.preventDefault(); this.editPersonaIndex = Math.max(0, this.editPersonaIndex - 1); }
      else if (e.key === "Enter") { e.preventDefault(); const p = matches[this.editPersonaIndex]; if (p) this.selectEditPersona(p); }
      else if (e.key === "Escape") { this.editPersonaOpen = false; }
    },

    onEditModelKeydown(e) {
      if (!this.editModelOpen) return;
      const matches = this.editModelMatches;
      if (e.key === "ArrowDown") { e.preventDefault(); this.editModelIndex = Math.min(matches.length - 1, this.editModelIndex + 1); }
      else if (e.key === "ArrowUp") { e.preventDefault(); this.editModelIndex = Math.max(0, this.editModelIndex - 1); }
      else if (e.key === "Enter") { e.preventDefault(); const m = matches[this.editModelIndex]; if (m) this.selectEditModel(m); }
      else if (e.key === "Escape") { this.editModelOpen = false; }
    },

    // editEffectiveParams drops null/empty fields from editParams so we don't
    // persist no-op overrides — same convention as the setup screen.
    get editEffectiveParams() {
      const out = {};
      for (const [k, v] of Object.entries(this.editParams)) {
        if (v !== null && v !== "" && v !== undefined && !Number.isNaN(v)) {
          out[k] = typeof v === "string" ? parseFloat(v) : v;
        }
      }
      return out;
    },

    async saveEdit() {
      const title = (this.editTitle || "").trim();
      if (!title) return; // server requires a non-empty title
      const id = this.editConvId;
      const model = (this.editModel || "").trim();
      const personaId = (this.editPersonaId || "").trim();
      const params = this.editEffectiveParams;
      // Optimistic local update so the sidebar title + the active chat's
      // persona/model/params reflect immediately. currentPersonaName and
      // currentModel read through current(), and streamTurn pulls params
      // from current().params — so updating the conversation object is
      // enough for the next send to pick up the new values.
      const conv = this.conversations.find((c) => c.id === id);
      if (conv) {
        conv.title = title;
        if (model) conv.model = model;
        if (personaId) conv.persona_id = personaId;
        conv.params = params;
      }
      if (this._serverMode) {
        fetch(`${this.prefix}/api/conversations/${id}`, {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ title, model, persona_id: personaId, params }),
        }).catch(() => {});
      } else {
        this.writeStorage();
      }
      this.showEditModal = false;
      this.editConvId = null;
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
      sessionStorage.removeItem("lmchatkit:currentId");
      // Note: autoAllowTools is NOT reset — "Always Allow" is
      // session-global, not per-chat.
    },

    startChat() {
      const id = "c-" + Math.random().toString(36).slice(2, 10);
      const persona = this.personas.find((p) => p.id === this.setupPersonaId) || { id: "default" };
      const conv = {
        id,
        title: "New conversation",
        persona_id: persona.id,
        model: this.setupModel,
        params: this.setupEffectiveParams,
        messages: [],
        created_at: Date.now(),
        updated_at: Date.now(),
      };
      // No system message — the server derives it from the persona
      // on each /api/chat request. The browser never stores or sends
      // system messages.
      this.conversations.unshift(conv);
      this.currentId = id;
      sessionStorage.setItem("lmchatkit:currentId", id);
      this.messages = conv.messages;
      if (!this._serverMode) {
        // sessionStorage mode needs the full conversation (with messages)
        // stored locally — server mode saves via persist() on first send.
        this.writeStorage();
      }
      this.$nextTick(() => this.$refs.composer && this.$refs.composer.focus());
    },

    async loadConversation(id) {
      // Observing a conversation clears its unread indicator (unread is
      // session-only; this is the only place it's cleared).
      const observed = this.conversations.find((c) => c.id === id);
      if (observed) observed.unread = false;

      if (this._serverMode) {
        try {
          const r = await fetch(`${this.prefix}/api/conversations/${id}`);
          if (!r.ok) return;
          const data = await r.json();
          this.currentId = id;
          sessionStorage.setItem("lmchatkit:currentId", id);
          this.messages = this.normalizeMessages(data.messages);
          this.userHasScrolled = false;
          this.scrollToBottom();
          this.focusComposer();
        } catch {}
      } else {
        const c = this.conversations.find((x) => x.id === id);
        if (!c) return;
        this.currentId = id;
        sessionStorage.setItem("lmchatkit:currentId", id);
        this.messages = c.messages;
        this.userHasScrolled = false;
        this.scrollToBottom();
        this.focusComposer();
      }
    },

    deleteCurrent() {
      if (!this.currentId) return;
      this.deleteConversation(this.currentId);
    },

    // deleteConversation opens the confirmation modal. The actual delete
    // happens in confirmDeleteChat when the user clicks Delete.
    deleteConversation(id) {
      if (!id) return;
      this.deleteChatTarget = id;
      this.showDeleteChatModal = true;
    },

    // confirmDeleteChat performs the deletion after the user confirms.
    confirmDeleteChat() {
      const id = this.deleteChatTarget;
      if (!id) return;
      this.showDeleteChatModal = false;
      this.deleteChatTarget = null;
      if (this._serverMode) {
        fetch(`${this.prefix}/api/conversations/${id}`, { method: "DELETE" }).catch(() => {});
      } else {
        this.conversations = this.conversations.filter((c) => c.id !== id);
        this.writeStorage();
      }
      if (id === this.currentId) {
        this.currentId = null;
        sessionStorage.removeItem("lmchatkit:currentId");
        this.messages = [];
      }
    },

    // renameConversation sends a title-only PATCH to the server and
    // updates the local sidebar immediately. The server broadcasts a
    // conversation_renamed SSE so other tabs pick up the new title.
    renameConversation(id, title) {
      title = (title || "").trim();
      if (!title) return;
      const conv = this.conversations.find((c) => c.id === id);
      if (conv) conv.title = title;
      if (this._serverMode) {
        fetch(`${this.prefix}/api/conversations/${id}`, {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ title }),
        }).catch(() => {});
      } else {
        this.writeStorage();
      }
    },

    persist() {
      const c = this.current;
      if (!c) return;
      if (this._serverMode) {
        const conv = {
          id: c.id,
          title: c.title,
          persona_id: c.persona_id,
          model: c.model,
          params: c.params,
          created_at: c.created_at || Date.now(),
          updated_at: Date.now(),
          messages: this.messages.map((m) => {
            const copy = { ...m };
            delete copy.showThinking;
            return copy;
          }),
        };
        // Update the local summary in-place.
        c.title = conv.title;
        c.updated_at = conv.updated_at;
        if (!c.persona_id) c.persona_id = conv.persona_id;
        if (!c.model) c.model = conv.model;
        // Tag this save so our own SSE event is ignored (prevents
        // loadConversationList from replacing the conversations array
        // with summaries mid-turn, which could lose unsaved data).
        this._lastSavedId = c.id;
        this._lastSavedAt = Date.now();
        fetch(`${this.prefix}/api/conversations/${c.id}`, {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(conv),
        }).catch(() => {});
      } else {
        c.messages = this.messages;
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
      if (this.promptMenuOpen) {
        e.preventDefault();
        const m = this.promptMenuMatches[this.promptMenuIndex];
        if (m) this.selectPromptFromMenu(m);
        return;
      }
      if (this.resourceMenuOpen) {
        e.preventDefault();
        const m = this.resourceMenuMatches[this.resourceMenuIndex];
        if (m) this.selectResourceFromMenu(m);
        return;
      }
      if (e.isComposing) return;
      if (e.shiftKey) return;
      e.preventDefault();
      this.send();
    },

    // -- input history (shell-style Up/Down) -----------------------------
    onArrowUp(e) {
      if (this.slashOpen) { e.preventDefault(); this.slashIndex = Math.max(0, this.slashIndex - 1); return; }
      if (this.promptMenuOpen) { e.preventDefault(); this.promptMenuIndex = Math.max(0, this.promptMenuIndex - 1); return; }
      if (this.resourceMenuOpen) { e.preventDefault(); this.resourceMenuIndex = Math.max(0, this.resourceMenuIndex - 1); return; }
      if (!this.shouldNavigateHistory("up", e.target)) return;
      e.preventDefault();
      this.navigateHistory("up");
    },
    onArrowDown(e) {
      if (this.slashOpen) { e.preventDefault(); this.slashIndex = Math.min(this.slashMatches.length - 1, this.slashIndex + 1); return; }
      if (this.promptMenuOpen) { e.preventDefault(); this.promptMenuIndex = Math.min(this.promptMenuMatches.length - 1, this.promptMenuIndex + 1); return; }
      if (this.resourceMenuOpen) { e.preventDefault(); this.resourceMenuIndex = Math.min(this.resourceMenuMatches.length - 1, this.resourceMenuIndex + 1); return; }
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

      this.userHasScrolled = false;
      this.recordInputHistory(draft);

      // Built-in meta commands (don't go to the model directly)
      if (draft === "/list-prompts") {
        this.draft = "";
        this.renderInfoCard("prompts", "Available MCP Prompts",
          (this.prompts || []).map((p) => ({
            name: "/" + p.name + (p.arguments && p.arguments.length ? " " + p.arguments.map(a => a.name + (a.required ? "*" : "")).join(" ") : ""),
            description: p.description || "",
          })));
        return;
      }
      if (draft === "/list-resources") {
        this.draft = "";
        this.renderInfoCard("resources", "Available MCP Resources",
          (this.resources || []).map((r) => ({
            name: r.uri,
            description: r.name || "",
            args: r.template ? "(template)" : "",
          })));
        return;
      }
      if (draft === "/compact") {
        this.draft = "";
        await this.compactConversation();
        return;
      }

      // File-based slash commands + MCP prompts share the /namespace.
      // File commands are checked first; if no match, fall through to MCP
      // prompts. Both are handled inside handleSlash.
      if (draft.startsWith("/")) {
        this.draft = "";
        const handled = await this.handleSlash(draft);
        if (handled) {
          return;
        }
      }

      // Normal message — @resource URIs in the text are resolved
      // server-side. The browser sends the text as-is; the server
      // reads the resources and builds content blocks. No client-side
      // fetch or attachment handling needed.
      const msg = { id: "msg-" + (++_msgSeq), role: "user", content: draft };
      this.messages.push(msg);
      this.draft = "";
      const c = this.current;
      if (c && c.title === "New conversation") {
        c.title = draft.slice(0, 50);
      }
      this.scrollToBottom();
      this.persist();
      await this.streamTurn();
    },

    // renderInfoCard pushes a synthetic assistant message with an `info`
    // object instead of text content. The template renders info cards
    // specially (no markdown, just a formatted list).
    renderInfoCard(kind, title, items) {
      this.messages.push({
        id: "msg-" + (++_msgSeq),
        role: "assistant",
        content: "",
        info: { kind, title, items },
      });
      this.scrollToBottom();
      this.persist();
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
        sessionStorage.setItem("lmchatkit:inputHistory", JSON.stringify(this.inputHistory));
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
        const persona = this.personas.find((p) => p.id === this.current?.persona_id);
        const params = this.current?.params || (persona && persona.params) || {};

        this.abortController = new AbortController();

        const r = await fetch(`${this.prefix}/api/chat`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            model: this.currentModel,
            persona_id: this.current?.persona_id || "",
            messages: this.messages.slice(0, -1),
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
            // Tools/prompts/resources changes are pushed via SSE events
            // (subscribeToEvents). In sessionStorage mode without SSE,
            // they refresh on page reload — acceptable since sessionStorage
            // is ephemeral.
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
        // Safety-net persist: guarantees the full conversation (including
        // the last assistant response) is saved even if earlier persist
        // calls raced with SSE-driven list refreshes.
        this.persist();
        this.focusComposer();
        this.scrollToBottom();
      }
    },

    // compactConversation sends the entire conversation to the LLM with a
    // summarization prompt, then replaces the message history with the
    // resulting summary. This reduces context window usage for long
    // conversations while preserving important details.
    //
    // The compacted result keeps:
    //   1. The original persona system prompt (so the model keeps its role)
    //   2. A new system message with the summary (hidden from the UI via
    //      role:"system" — the template skips system messages)
    //   3. An info card showing what was compacted (visible to the user)
    async compactConversation() {
      if (this.streaming) return;
      const conversational = this.messages.filter((m) => !m.info);
      if (conversational.length < 2) {
        this.renderInfoCard("info", "Nothing to Compact",
          [{ name: "Need at least 2 messages", description: "Compaction is useful for longer conversations." }]);
        return;
      }

      // Pack the entire conversation into a single user message so the
      // model treats this as a summarization task, NOT a live conversation
      // to continue. Passing messages as separate user/assistant turns
      // causes the model to respond to the last message rather than
      // summarize — and it can run for thousands of tokens.
      const extractText = (content) => {
        if (typeof content === "string") return content;
        if (Array.isArray(content)) return content.map((b) => b.text || "").join("");
        return "";
      };
      const transcript = conversational.map((m) => {
        const label = m.role === "user" ? "User" : "Assistant";
        return `[${label}]: ${extractText(m.content)}`;
      }).join("\n\n");

      const compactPrompt =
        "Summarize the following conversation concisely (200-500 words). " +
        "Preserve key decisions, code snippets, file paths, URLs, and unresolved " +
        "questions. Omit pleasantries and small talk. The summary must be " +
        "detailed enough to continue the conversation seamlessly.";

      // Pack prompt + transcript into a single user message. The server
      // injects the persona's system prompt — we don't send system messages.
      const compactMessages = [
        { role: "user", content: compactPrompt + "\n\nSummarize this conversation:\n\n" + transcript },
      ];

      // Push an empty assistant bubble so the loading dots have somewhere
      // to render. This bubble is replaced when compaction completes.
      this.messages.push({
        id: "msg-" + (++_msgSeq), role: "assistant",
        content: "", thinking: "", tool_calls: [],
      });
      const compactingBubble = this.messages[this.messages.length - 1];

      this.streaming = true;
      this.abortController = new AbortController();
      this.userHasScrolled = false;
      this.scrollToBottom();

      try {
        const r = await fetch(`${this.prefix}/api/chat`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            model: this.currentModel,
            messages: compactMessages,
            tools: [],
            params: this.current?.params || {},
          }),
          signal: this.abortController.signal,
        });

        if (!r.ok) {
          // Remove the temporary bubble before showing the error card.
          const idx = this.messages.indexOf(compactingBubble);
          if (idx >= 0) this.messages.splice(idx, 1);
          const err = await r.json().catch(() => ({ error: "compaction failed" }));
          this.renderInfoCard("error", "Compaction Failed",
            [{ name: err.error || "Unknown error", description: "" }]);
          return;
        }

        let summary = "";
        await this.readSSE(r, (ev) => {
          if (ev.type === "delta") {
            summary += ev.delta;
          } else if (ev.type === "error") {
            summary = "";
          }
        });

        if (!summary.trim()) {
          const idx = this.messages.indexOf(compactingBubble);
          if (idx >= 0) this.messages.splice(idx, 1);
          this.renderInfoCard("error", "Compaction Failed",
            [{ name: "Empty summary returned", description: "" }]);
          return;
        }

        const oldCount = this.messages.length - 1; // exclude the temp bubble

        // Replace messages with just the summary. The system prompt is
        // not stored — the server derives it from the persona on each
        // request, so compaction doesn't need to touch it.
        this.messages = [{
          id: "msg-" + (++_msgSeq),
          role: "assistant",
          content: summary,
          thinking: "",
          tool_calls: [],
        }];
        // The info card tells the user what happened — the assistant
        // message itself is just the raw summary so the model sees clean
        // context, not meta-commentary about compaction.
        this.renderInfoCard("compact", "Conversation Compacted",
          [{ name: oldCount + " messages \u2192 summary", description: "Context reduced. The model retains the key details above." }]);
      } catch (e) {
        // Clean up the temporary bubble on any error.
        const idx = this.messages.indexOf(compactingBubble);
        if (idx >= 0) this.messages.splice(idx, 1);
        if (e && e.name === "AbortError") return;
        this.renderInfoCard("error", "Compaction Failed",
          [{ name: (e && e.message) || "Unknown error", description: "" }]);
      } finally {
        this.streaming = false;
        this.abortController = null;
        this.persist();
        this.focusComposer();
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
        sessionStorage.setItem("lmchatkit:autoAllow", JSON.stringify(this.autoAllowTools));
      }
      call.approval = "approving";
      await this.executeToolCall(call);
      await this.maybeResumeAfterApproval();
    },

    async denyToolCall(call) {
      if (call.approval !== "pending") return;
      call.approval = "denying";
      call.result = "Denied by user";
      // The tool message content is what the model sees. Be explicit so
      // the LLM understands this is a hard "no" and doesn't retry.
      const denyMsg = "The user denied this tool call. Do not attempt to call '" + call.name + "' again. " +
        "Ask the user how they would like to proceed, or continue with an alternative approach.";
      this.messages.push({
        id: "msg-" + (++_msgSeq), role: "tool",
        tool_call_id: call.id,
        tool_name: call.name,
        content: denyMsg,
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
      const builtins = [
        { id: "_compact", name: "compact", description: "Summarize conversation history to save context", _builtin: true },
        { id: "_list-prompts", name: "list-prompts", description: "List available MCP prompts", _builtin: true },
        { id: "_list-resources", name: "list-resources", description: "List available MCP resources", _builtin: true },
      ];
      // MCP prompts merge into the namespace as slash commands.
      const promptCmds = (this.prompts || []).map((p) => ({
        id: "prompt:" + p.name,
        name: p.name,
        description: p.description || "",
        argument_hint: (p.arguments || []).map((a) => a.name + (a.required ? "*" : "")).join(" "),
        _isPrompt: true,
      }));
      const all = [...builtins, ...(this.commands || []), ...promptCmds];
      if (!name) return all;
      return all.filter((c) => c.name.startsWith(name));
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
      // MCP prompt: if it declares arguments, wait for the user to type
      // them. If not, submit immediately.
      if (cmd._isPrompt) {
        if (cmd.argument_hint) {
          this.draft = "/" + cmd.name + " ";
          this.$nextTick(() => this.$refs.composer && this.$refs.composer.focus());
          return;
        }
        this.draft = "/" + cmd.name;
        this.send();
        return;
      }
      // File-based or built-in: if the command body doesn't use $ARGUMENTS
      // (or has no body), submit immediately.
      if (!cmd.body || !cmd.body.includes("$ARGUMENTS")) {
        this.draft = "/" + cmd.name;
        this.send();
        return;
      }
      this.draft = "/" + cmd.name + " ";
      this.$nextTick(() => this.$refs.composer && this.$refs.composer.focus());
    },

    // -- /prompt autocomplete ---------------------------------------------
    get promptMenuMatches() {
      if (!this.draft.startsWith("/prompt ")) return [];
      const rest = this.draft.slice(8).trim();
      if (rest.includes(" ")) return [];
      if (!rest) return (this.prompts || []).slice();
      return (this.prompts || []).filter((p) => p.name.toLowerCase().startsWith(rest.toLowerCase()));
    },

    selectPromptFromMenu(p) {
      this.draft = "/prompt " + p.name + " ";
      this.promptMenuOpen = false;
      this.$nextTick(() => this.$refs.composer && this.$refs.composer.focus());
    },

    // -- @resource autocomplete -------------------------------------------
    get resourceMenuMatches() {
      const m = this.draft.match(/@([\w:/.{}-]*)$/);
      if (!m) return [];
      const partial = m[1].toLowerCase();
      // Show all resources — static and template. Template resources
      // ({var} placeholders) are matched on their scheme prefix so
      // @greet finds greeting://{name} even though {name} != "greet".
      const all = this.resources || [];
      if (!partial) return all.slice(0, 10);
      return all.filter((r) => {
        // For templates, match against the prefix before the first {.
        const matchUri = r.uri.includes("{")
          ? r.uri.substring(0, r.uri.indexOf("{")).toLowerCase()
          : r.uri.toLowerCase();
        return matchUri.includes(partial) || (r.name || "").toLowerCase().includes(partial);
      }).slice(0, 10);
    },

    selectResourceFromMenu(r) {
      this.resourceMenuOpen = false;
      // Static: insert the full URI (@docs://test.md).
      // Template: insert everything up to the first {var} and leave
      // the cursor for the user to type the value
      // (@greeting:// — user types "Ada" → @greeting://Ada).
      let insert;
      if (r.uri.includes("{")) {
        insert = r.uri.substring(0, r.uri.indexOf("{"));
      } else {
        insert = r.uri;
      }
      this.draft = this.draft.replace(/@([\w:/.{}-]*)$/, "@" + insert);
      this.$nextTick(() => this.$refs.composer && this.$refs.composer.focus());
    },

    async handleSlash(input) {
      const trimmed = input.trim();
      const sp = trimmed.indexOf(" ");
      const name = (sp === -1 ? trimmed.slice(1) : trimmed.slice(1, sp)).toLowerCase();
      const args = sp === -1 ? "" : trimmed.slice(sp + 1).trim();

      // 1. File-based slash commands take precedence.
      const cmd = (this.commands || []).find((c) => c.name === name);
      if (cmd) {
        if (cmd.allowed_tools) {
          for (const raw of cmd.allowed_tools.split(",")) {
            const toolName = raw.trim().split("(")[0].trim();
            if (toolName && !this.autoAllowTools.includes(toolName)) {
              this.autoAllowTools.push(toolName);
            }
          }
          sessionStorage.setItem("lmchatkit:autoAllow", JSON.stringify(this.autoAllowTools));
        }
        const rendered = (cmd.body || "").replaceAll("$ARGUMENTS", args).trim();
        this.messages.push({ id: "msg-" + (++_msgSeq), role: "user", content: rendered });
        this.scrollToBottom();
        this.persist();
        await this.streamTurn();
        return true;
      }

      // 2. MCP prompts: resolved server-side. Send the raw /name args
      // text as a user message — the server calls prompts/get and
      // injects the rendered messages before forwarding to the LLM.
      // The transcript shows the original /name args text.
      return false;
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

    copyMessage(idx, content) {
      if (!content) return;
      navigator.clipboard.writeText(content).then(() => {
        this.copiedIdx = idx;
        setTimeout(() => { if (this.copiedIdx === idx) this.copiedIdx = -1; }, 2000);
      }).catch(() => {});
    },

    copyMessageHtml(idx, content) {
      if (!content) return;
      const html = this.formatContent(content);
      const container = document.createElement('div');
      container.innerHTML = html;
      container.style.position = 'fixed';
      container.style.left = '-9999px';
      container.style.top = '0';
      document.body.appendChild(container);
      const range = document.createRange();
      range.selectNodeContents(container);
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      try {
        document.execCommand('copy');
        this.copiedHtmlIdx = idx;
        setTimeout(() => { if (this.copiedHtmlIdx === idx) this.copiedHtmlIdx = -1; }, 2000);
      } catch (e) {
        this.copyMessage(idx, content);
      }
      sel.removeAllRanges();
      document.body.removeChild(container);
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
