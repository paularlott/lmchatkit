package lmchatkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newGuardedMux mounts a Server with every optional route (upload,
// conversation history) so the CSRF guard can be exercised end to end.
func newGuardedMux(t *testing.T, cfg Config, called *int) *http.ServeMux {
	t.Helper()
	cfg.Prefix = "/chat"
	cfg.FileUpload = true
	cfg.History = &memoryHistoryTestStore{data: map[string]*StoredConversation{}}
	cfg.Host = &fakeHost{
		tools: []Tool{{Name: "ping"}},
		callTool: func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
			*called++
			return ToolResult{Content: "pong"}, nil
		},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	s.Mount(mux)
	return mux
}

// TestCSRFGuard_ToolCallMatrix pins the accept/reject rules of csrfGuard on
// /api/tools/call: Content-Type must be application/json, and a browser
// request (Origin / Sec-Fetch-Site present) must be same-origin with the
// request host or from Config.AllowedOrigins. A refused request must never
// reach the host.
func TestCSRFGuard_ToolCallMatrix(t *testing.T) {
	const body = `{"name":"ping","arguments":{}}`
	for _, tc := range []struct {
		name    string
		cfg     Config
		host    string // request Host; default example.com
		headers map[string]string
		want    int
	}{
		{name: "API client, JSON, no Origin", headers: map[string]string{"Content-Type": "application/json"}, want: 200},
		{name: "JSON with charset", headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, want: 200},
		{name: "text/plain refused", headers: map[string]string{"Content-Type": "text/plain"}, want: 415},
		{name: "form-urlencoded refused", headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, want: 415},
		{name: "no Content-Type refused", headers: map[string]string{}, want: 415},
		{name: "same-origin Origin", headers: map[string]string{"Content-Type": "application/json", "Origin": "http://example.com"}, want: 200},
		{name: "same host, other scheme (TLS at proxy)", headers: map[string]string{"Content-Type": "application/json", "Origin": "https://example.com"}, want: 200},
		{name: "sibling subdomain refused", headers: map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example.com"}, want: 403},
		{name: "other port refused", headers: map[string]string{"Content-Type": "application/json", "Origin": "http://example.com:8443"}, want: 403},
		{name: "text/plain from sibling subdomain refused", headers: map[string]string{"Content-Type": "text/plain", "Origin": "https://evil.example.com"}, want: 403},
		{name: "Origin null (sandboxed iframe) refused", headers: map[string]string{"Content-Type": "application/json", "Origin": "null"}, want: 403},
		{name: "Sec-Fetch-Site same-origin", headers: map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "same-origin"}, want: 200},
		{name: "Sec-Fetch-Site none", headers: map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "none"}, want: 200},
		{name: "Sec-Fetch-Site same-site refused", headers: map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "same-site"}, want: 403},
		{name: "Sec-Fetch-Site cross-site refused", headers: map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}, want: 403},
		{name: "Sec-Fetch-Site beats a matching Origin", headers: map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site", "Origin": "http://example.com"}, want: 403},
		{
			name:    "AllowedOrigins admits a listed origin",
			cfg:     Config{AllowedOrigins: []string{"https://app.example.com/"}},
			headers: map[string]string{"Content-Type": "application/json", "Origin": "https://app.example.com", "Sec-Fetch-Site": "same-site"},
			want:    200,
		},
		{
			name:    "AllowedOrigins does not admit others",
			cfg:     Config{AllowedOrigins: []string{"https://app.example.com"}},
			headers: map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example.com"},
			want:    403,
		},
		{
			name:    "proxy rewrote Host, no trust: refused",
			host:    "backend:8080",
			headers: map[string]string{"Content-Type": "application/json", "Origin": "https://chat.example.com", "X-Forwarded-Host": "chat.example.com"},
			want:    403,
		},
		{
			name:    "proxy rewrote Host, TrustForwardedHost",
			cfg:     Config{TrustForwardedHost: true},
			host:    "backend:8080",
			headers: map[string]string{"Content-Type": "application/json", "Origin": "https://chat.example.com", "X-Forwarded-Host": "chat.example.com, inner"},
			want:    200,
		},
		{
			name:    "TrustForwardedHost still refuses other origins",
			cfg:     Config{TrustForwardedHost: true},
			host:    "backend:8080",
			headers: map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example.com", "X-Forwarded-Host": "chat.example.com"},
			want:    403,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := 0
			mux := newGuardedMux(t, tc.cfg, &called)
			req := httptest.NewRequest(http.MethodPost, "/chat/api/tools/call", strings.NewReader(body))
			if tc.host != "" {
				req.Host = tc.host
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if wantCalled := tc.want == 200; (called == 1) != wantCalled {
				t.Fatalf("host CallTool called %d times, want called=%v", called, wantCalled)
			}
		})
	}
}

// TestCSRFGuard_EveryStateChangingRoute checks the guard is on every POST,
// PUT, PATCH and DELETE route Mount registers, and off GET routes.
func TestCSRFGuard_EveryStateChangingRoute(t *testing.T) {
	called := 0
	mux := newGuardedMux(t, Config{}, &called)
	serve := func(method, path, ctype, origin string, body []byte) int {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	jsonRoutes := []struct{ method, path string }{
		{http.MethodPost, "/chat/api/chat"},
		{http.MethodPost, "/chat/api/tools/call"},
		{http.MethodPost, "/chat/api/prompts/get"},
		{http.MethodPost, "/chat/api/resources/read"},
		{http.MethodPost, "/chat/api/app-proxy"},
		{http.MethodPut, "/chat/api/conversations/c1"},
		{http.MethodPatch, "/chat/api/conversations/c1"},
	}
	for _, rt := range jsonRoutes {
		if got := serve(rt.method, rt.path, "text/plain", "", []byte(`{}`)); got != http.StatusUnsupportedMediaType {
			t.Errorf("%s %s text/plain: status %d, want 415", rt.method, rt.path, got)
		}
		if got := serve(rt.method, rt.path, "application/json", "https://evil.example.com", []byte(`{}`)); got != http.StatusForbidden {
			t.Errorf("%s %s cross-origin: status %d, want 403", rt.method, rt.path, got)
		}
	}

	// DELETE: no body, so no Content-Type rule — the origin rule alone.
	if got := serve(http.MethodDelete, "/chat/api/conversations/c1", "", "https://evil.example.com", nil); got != http.StatusForbidden {
		t.Errorf("DELETE cross-origin: status %d, want 403", got)
	}
	if got := serve(http.MethodDelete, "/chat/api/conversations/c1", "", "http://example.com", nil); got == http.StatusForbidden || got == http.StatusUnsupportedMediaType {
		t.Errorf("DELETE same-origin: status %d, want it through the guard", got)
	}

	// Upload: multipart/form-data, origin-checked.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.txt")
	fw.Write([]byte("hello"))
	mw.Close()
	if got := serve(http.MethodPost, "/chat/api/upload", mw.FormDataContentType(), "http://example.com", buf.Bytes()); got != http.StatusOK {
		t.Errorf("upload same-origin multipart: status %d, want 200", got)
	}
	if got := serve(http.MethodPost, "/chat/api/upload", mw.FormDataContentType(), "https://evil.example.com", buf.Bytes()); got != http.StatusForbidden {
		t.Errorf("upload cross-origin multipart: status %d, want 403", got)
	}
	if got := serve(http.MethodPost, "/chat/api/upload", "application/json", "", []byte(`{}`)); got != http.StatusUnsupportedMediaType {
		t.Errorf("upload as JSON: status %d, want 415", got)
	}

	// GET routes are untouched, even with a foreign Origin.
	if got := serve(http.MethodGet, "/chat/api/config", "", "https://evil.example.com", nil); got != http.StatusOK {
		t.Errorf("GET config cross-origin: status %d, want 200", got)
	}
	if called != 0 {
		t.Errorf("host CallTool reached %d times by refused requests", called)
	}
}

// alwaysConfirmHost is a host with an always-confirm tool, a discoverable
// always-confirm tool (only in ListAllTools), a plain tool, and a view mount.
func alwaysConfirmHost(dispatched *[]string) *fakeAllToolsHost {
	tools := []Tool{
		{Name: "mount", Visibility: []string{"app"}},
		{Name: "run_script", AlwaysConfirm: ConfirmRule{"mode": "write"}},
		{Name: "wipe", AlwaysConfirm: ConfirmRule{}},
		{Name: "list_things"},
		{Name: "execute_tool"},
	}
	return &fakeAllToolsHost{
		fakeHost: &fakeHost{
			tools: tools,
			callTool: func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
				*dispatched = append(*dispatched, name)
				return ToolResult{Content: "ok:" + name}, nil
			},
		},
		allTools: append(append([]Tool{}, tools...), Tool{Name: "hidden_wipe", AlwaysConfirm: ConfirmRule{}}),
	}
}

// TestAppProxy_RefusesAlwaysConfirmTools pins that an MCP App view cannot
// call a tool whose always-confirm rule picks the call — directly, through
// execute_tool (any argument form, any nesting), or a discoverable tool —
// since a view's calls never get an approval card. Calls the rule does not
// pick still go through.
func TestAppProxy_RefusesAlwaysConfirmTools(t *testing.T) {
	var dispatched []string
	s := newTestServer(t, alwaysConfirmHost(&dispatched))

	for _, tc := range []struct {
		name, params string
		want         int
	}{
		{"rule match", `{"name":"run_script","arguments":{"mode":"write","code":"x"}}`, 403},
		{"rule with no arguments (every call)", `{"name":"wipe","arguments":{}}`, 403},
		{"discoverable always-confirm tool", `{"name":"hidden_wipe","arguments":{}}`, 403},
		{"execute_tool, parameters object", `{"name":"execute_tool","arguments":{"name":"run_script","parameters":{"mode":"write"}}}`, 403},
		{"execute_tool, parameters string", `{"name":"execute_tool","arguments":{"name":"run_script","parameters":"{\"mode\":\"write\"}"}}`, 403},
		{"execute_tool, legacy arguments key", `{"name":"execute_tool","arguments":{"name":"run_script","arguments":{"mode":"write"}}}`, 403},
		{"execute_tool nested", `{"name":"execute_tool","arguments":{"name":"execute_tool","parameters":{"name":"wipe","parameters":{}}}}`, 403},
		{"execute_tool, unreadable wrapped arguments", `{"name":"execute_tool","arguments":{"name":"run_script","parameters":{},"arguments":{}}}`, 403},
		{"rule does not match", `{"name":"run_script","arguments":{"mode":"read"}}`, 200},
		{"plain tool", `{"name":"list_things","arguments":{}}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dispatched = nil
			body := `{"method":"tools/call","params":` + tc.params + `,"via":"mount"}`
			rec := httptest.NewRecorder()
			s.handleAppProxy(rec, httptest.NewRequest(http.MethodPost, "/chat/api/app-proxy", strings.NewReader(body)))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == 403 {
				if !strings.Contains(rec.Body.String(), "confirmation") {
					t.Fatalf("refused, but not by the always-confirm check: %s", rec.Body.String())
				}
				if len(dispatched) != 0 {
					t.Fatalf("refused call reached the host: %v", dispatched)
				}
			}
		})
	}
}

// TestAppProxy_AlwaysConfirmFailsClosed: if the tool list cannot be read,
// the always-confirm rules are unknown, so a view's call is refused.
func TestAppProxy_AlwaysConfirmFailsClosed(t *testing.T) {
	var dispatched []string
	host := alwaysConfirmHost(&dispatched)
	host.listToolsErr = errors.New("down")
	s := newTestServer(t, host)
	rec := httptest.NewRecorder()
	body := `{"method":"tools/call","params":{"name":"list_things","arguments":{}},"via":"mount"}`
	s.handleAppProxy(rec, httptest.NewRequest(http.MethodPost, "/chat/api/app-proxy", strings.NewReader(body)))
	if rec.Code != http.StatusForbidden || len(dispatched) != 0 {
		t.Fatalf("status = %d, dispatched %v; want 403 and nothing dispatched", rec.Code, dispatched)
	}
}

// TestCallTool_AlwaysConfirmBySource: on /api/tools/call, an app-sourced
// call an always-confirm rule picks is refused (directly or through
// execute_tool), while the same call from the model path — which the chat
// UI only sends after the person approves its card — runs as before.
func TestCallTool_AlwaysConfirmBySource(t *testing.T) {
	var dispatched []string
	s := newTestServer(t, alwaysConfirmHost(&dispatched))

	for _, tc := range []struct {
		name, body string
		want       int
		dispatch   string
	}{
		{"app: rule match refused", `{"name":"run_script","arguments":{"mode":"write"},"source":"app","via":"mount"}`, 403, ""},
		{"app: execute_tool-wrapped refused", `{"name":"execute_tool","arguments":{"name":"run_script","parameters":{"mode":"write"}},"source":"app","via":"mount"}`, 403, ""},
		{"app: discoverable refused", `{"name":"hidden_wipe","arguments":{},"source":"app","via":"mount"}`, 403, ""},
		{"app: rule does not match", `{"name":"run_script","arguments":{"mode":"read"},"source":"app","via":"mount"}`, 200, "run_script"},
		{"model: rule match runs", `{"name":"run_script","arguments":{"mode":"write"}}`, 200, "run_script"},
		{"model: execute_tool-wrapped runs", `{"name":"execute_tool","arguments":{"name":"run_script","parameters":{"mode":"write"}}}`, 200, "execute_tool"},
		{"model: every-call rule runs", `{"name":"wipe","arguments":{}}`, 200, "wipe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dispatched = nil
			rec := httptest.NewRecorder()
			s.handleCallTool(rec, httptest.NewRequest(http.MethodPost, "/chat/api/tools/call", strings.NewReader(tc.body)))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == 403 && !strings.Contains(rec.Body.String(), "confirmation") {
				t.Fatalf("refused, but not by the always-confirm check: %s", rec.Body.String())
			}
			if got := strings.Join(dispatched, ","); got != tc.dispatch {
				t.Fatalf("dispatched %q, want %q", got, tc.dispatch)
			}
		})
	}
}
