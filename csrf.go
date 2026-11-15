package lmchatkit

import (
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// requestBody names the body a state-changing route accepts, for the
// Content-Type half of [Server.csrfGuard].
type requestBody int

const (
	// bodyJSON routes take a JSON body and require Content-Type
	// application/json. A browser cannot send that type cross-origin without
	// a CORS preflight, which lmchatkit never answers.
	bodyJSON requestBody = iota
	// bodyMultipart routes (the file upload) require multipart/form-data. A
	// cross-origin form can send that type with no preflight, so these
	// routes rely on the Origin / Sec-Fetch-Site check alone.
	bodyMultipart
	// bodyNone routes (DELETE) take no body; any Content-Type is accepted.
	// DELETE is never a CORS-simple method, so it is preflighted anyway.
	bodyNone
)

// csrfGuard wraps a state-changing handler (every method but GET, HEAD and
// OPTIONS) with cross-site request forgery protection. lmchatkit's routes
// usually sit behind a cookie session, and a SameSite=Lax cookie still rides
// along on a POST from a page on a sibling subdomain (same site, different
// origin), so without this a page on such a subdomain could POST text/plain
// JSON to /api/tools/call and run tools as the signed-in person.
//
// A request is refused with 403 unless its origin passes, in order:
//
//  1. An Origin header listed in Config.AllowedOrigins is accepted.
//  2. When Sec-Fetch-Site is present (every current browser sends it), it
//     must be "same-origin" or "none" (user-initiated).
//  3. With no Origin header either, the request is accepted: it did not come
//     from a browser page (browsers send Origin on every cross-origin POST,
//     PUT, PATCH and DELETE), so API clients keep working.
//  4. Otherwise the Origin's host[:port] must equal the request's host — the
//     Host header, or the first X-Forwarded-Host value when
//     Config.TrustForwardedHost is set. The scheme is not compared (the same
//     rule as net/http's CrossOriginProtection), so TLS terminated at a proxy
//     needs no extra configuration. "Origin: null" (a sandboxed iframe, such
//     as an MCP App view) never matches.
//
// and then its Content-Type must fit the route (see [requestBody]), else 415.
func (s *Server) csrfGuard(h http.Handler, body requestBody) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			h.ServeHTTP(w, r)
			return
		}
		if !s.sameOriginRequest(r) {
			writeError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		switch body {
		case bodyJSON:
			if !hasMediaType(r, "application/json") {
				writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		case bodyMultipart:
			if !hasMediaType(r, "multipart/form-data") {
				writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be multipart/form-data")
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// sameOriginRequest applies the origin rules documented on csrfGuard.
func (s *Server) sameOriginRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin != "" {
		for _, allowed := range s.cfg.AllowedOrigins {
			if strings.EqualFold(strings.TrimRight(allowed, "/"), origin) {
				return true
			}
		}
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, s.requestHost(r))
}

// requestHost is the host[:port] the browser addressed: the Host header, or
// the proxy-supplied X-Forwarded-Host when the host trusts its proxy.
func (s *Server) requestHost(r *http.Request) string {
	if s.cfg.TrustForwardedHost {
		if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
			first, _, _ := strings.Cut(fwd, ",")
			if first = strings.TrimSpace(first); first != "" {
				return first
			}
		}
	}
	return r.Host
}

// hasMediaType reports whether the request's Content-Type is want,
// ignoring parameters such as charset or boundary.
func hasMediaType(r *http.Request, want string) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mt, want)
}
