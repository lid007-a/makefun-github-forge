// app.go — HTTP server: routing, auth, body handling. Port of app.ts.
//
// Surfaces:
//   GET  /healthz                                  (no auth; the boot probe)
//   /{owner}/{repo}.git/info/refs                 git smart-HTTP advertisement
//   POST /{owner}/{repo}.git/git-upload-pack       upload-pack v0/v2 + ls-refs
//   POST /{owner}/{repo}.git/git-receive-pack      receive-pack v0
//   /api/v3/*                                      REST v3 (JSON)
//   POST /api/graphql                             GraphQL v4 (JSON)
//   /mcp                                          MCP JSON-RPC 2.0 (JSON)
//
// Auth: every surface except /healthz requires `Authorization: Bearer
// <token>`. When MAKEFUN_SHOP_TOKEN is set the token must match it;
// otherwise any non-empty Bearer token is accepted (local testing).
// A repo under any owner other than the token owner answers 404, never 403.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const maxBodyBytes = 64 * 1024 * 1024

func shopToken() string { return os.Getenv("MAKEFUN_SHOP_TOKEN") }

func isAuthorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if h == "" || !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(h[len("Bearer "):])
	if token == "" {
		return false
	}
	expected := shopToken()
	return expected == "" || token == expected
}

// marshalJSON renders a value as JSON the way the Node port does:
// no HTML escaping of <, >, & (encoding/json escapes them by default).
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoder adds a trailing newline; strip it.
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

func readBody(r *http.Request) ([]byte, error) {
	// Read at most maxBodyBytes+1 so an over-large body is detected.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		return nil, errBodyTooLarge
	}
	return body, nil
}

var errBodyTooLarge = &bodyTooLargeError{}

type bodyTooLargeError struct{}

func (e *bodyTooLargeError) Error() string { return "request body too large" }

var gitRouteRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(.+)$`)

func writeJSON(w http.ResponseWriter, status int, body any, requestID string, extra map[string]string) {
	var payload []byte
	if body != nil {
		payload = marshalJSON(body)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Github-Request-Id", requestID)
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(payload)
}

func writeBuffer(w http.ResponseWriter, status int, contentType string, body []byte, requestID string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Github-Request-Id", requestID)
	w.WriteHeader(status)
	w.Write(body)
}

func writeText(w http.ResponseWriter, status int, body, requestID string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain")
	h.Set("X-Github-Request-Id", requestID)
	w.WriteHeader(status)
	w.Write([]byte(body))
}

// makeHandler returns the HTTP handler for the forge. Every request gets a
// request id; a panic in a handler is caught and answered 400, never
// crashing the process.
func makeHandler(store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := store.requestID()
		defer func() {
			if rec := recover(); rec != nil {
				// Never crash the process on a bad request; answer 400 and stay up.
				// (If headers were already sent, there is nothing to write.)
				func() {
					defer func() { _ = recover() }()
					msg := "Bad Request"
					if err, ok := rec.(error); ok {
						msg = err.Error()
					} else if s, ok := rec.(string); ok {
						msg = s
					}
					writeJSON(w, 400, map[string]any{"message": msg}, requestID, nil)
				}()
			}
		}()

		// Use the escaped path so %2F inside a segment does not split it.
		path := r.URL.EscapedPath()
		method := r.Method

		// Boot probe: no auth.
		if path == "/healthz" {
			writeText(w, 200, "ok", requestID)
			return
		}

		if !isAuthorized(r) {
			if strings.HasSuffix(path, ".git/info/refs") || strings.Contains(path, ".git/") {
				writeText(w, 401, "Unauthorized", requestID)
			} else {
				writeJSON(w, 401, map[string]any{"message": "Requires authentication"}, requestID, nil)
			}
			return
		}

		// ---- git smart HTTP ----
		if m := gitRouteRe.FindStringSubmatch(path); m != nil {
			owner, _ := url.PathUnescape(m[1])
			repoName, _ := url.PathUnescape(m[2])
			action := m[3]
			var repo *Repo
			if owner == ViewerLogin {
				store.mu.RLock()
				repo = store.getRepo(owner, repoName)
				store.mu.RUnlock()
			}
			if repo == nil {
				writeText(w, 404, "Not Found", requestID)
				return
			}
			if action == "info/refs" {
				service := r.URL.Query().Get("service")
				if service == "git-upload-pack" {
					version2 := r.Header.Get("Git-Protocol") == "version=2"
					store.mu.RLock()
					body := infoRefsUploadPack(repo, version2)
					store.mu.RUnlock()
					writeBuffer(w, 200, "application/x-"+service+"-advertisement", body, requestID)
					return
				}
				if service == "git-receive-pack" {
					store.mu.RLock()
					body := infoRefsReceivePack(repo)
					store.mu.RUnlock()
					writeBuffer(w, 200, "application/x-"+service+"-advertisement", body, requestID)
					return
				}
				writeText(w, 400, "Bad Request", requestID)
				return
			}
			if action == "git-upload-pack" && method == "POST" {
				body, err := readBody(r)
				if err != nil {
					writeJSON(w, 400, map[string]any{"message": err.Error()}, requestID, nil)
					return
				}
				version2 := r.Header.Get("Git-Protocol") == "version=2"
				var out gitResult
				head := body
				if len(head) > 64 {
					head = head[:64]
				}
				if version2 && bytes.Contains(head, []byte("command=ls-refs")) {
					out = handleLsRefs(store, repo, body)
				} else {
					out = handleUploadPack(store, repo, body, version2)
				}
				writeBuffer(w, out.status, out.contentType, out.body, requestID)
				return
			}
			if action == "git-receive-pack" && method == "POST" {
				body, err := readBody(r)
				if err != nil {
					writeJSON(w, 400, map[string]any{"message": err.Error()}, requestID, nil)
					return
				}
				out := handleReceivePack(store, repo, body)
				writeBuffer(w, out.status, out.contentType, out.body, requestID)
				return
			}
			writeText(w, 404, "Not Found", requestID)
			return
		}

		// ---- GraphQL ----
		if path == "/api/graphql" {
			if method != "POST" {
				writeJSON(w, 405, map[string]any{"message": "Method Not Allowed"}, requestID, nil)
				return
			}
			body, err := readBody(r)
			if err != nil {
				writeJSON(w, 400, map[string]any{"message": err.Error()}, requestID, nil)
				return
			}
			status, out := handleGraphQL(store, body)
			writeJSON(w, status, out, requestID, nil)
			return
		}

		// ---- MCP ----
		if path == "/mcp" {
			if method == "DELETE" {
				w.Header().Set("X-Github-Request-Id", requestID)
				w.WriteHeader(204)
				return
			}
			if method != "POST" {
				writeJSON(w, 405, map[string]any{"message": "Method Not Allowed"}, requestID, nil)
				return
			}
			body, err := readBody(r)
			if err != nil {
				writeJSON(w, 400, map[string]any{"message": err.Error()}, requestID, nil)
				return
			}
			headers := make(map[string]string)
			for k, vs := range r.Header {
				if len(vs) > 0 {
					headers[strings.ToLower(k)] = vs[0]
				}
			}
			out := handleMcp(store, body, headers)
			if s, ok := out.body.(string); ok {
				h := w.Header()
				h.Set("Content-Type", "application/json")
				h.Set("X-Github-Request-Id", requestID)
				for k, v := range out.headers {
					h.Set(k, v)
				}
				w.WriteHeader(out.status)
				w.Write([]byte(s))
				return
			}
			writeJSON(w, out.status, out.body, requestID, out.headers)
			return
		}

		// ---- REST v3 ----
		if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
			var body []byte
			if method != "GET" && method != "DELETE" {
				var err error
				body, err = readBody(r)
				if err != nil {
					writeJSON(w, 400, map[string]any{"message": err.Error()}, requestID, nil)
					return
				}
			} else {
				body = []byte{}
			}
			origin := "http://" + r.Host
			if r.Host == "" {
				origin = "http://127.0.0.1"
			}
			out := handleRest(store, method, path, r.URL.Query(), body, origin)
			if out.status == 204 {
				w.Header().Set("X-Github-Request-Id", requestID)
				w.WriteHeader(204)
				return
			}
			writeJSON(w, out.status, out.body, requestID, out.headers)
			return
		}

		// Unknown route: JSON 404, never HTML.
		writeJSON(w, 404, map[string]any{"message": "Not Found"}, requestID, nil)
	})
}
