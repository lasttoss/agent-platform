package main

// mcpfront serves the gateway to an MCP client (DeepSeek Harness and anything else that
// speaks Streamable HTTP). It is the same pipeline as /v1/respond with one difference that
// matters: the caller is an MCP client, so identity must arrive in the transport — a bearer
// token that maps to a user — and never as a tool argument the model could choose.
//
// Three things happen here, in this order: resolve identity, filter what the model may see,
// then (for a call) decide, cache, forward, record. Filtering before the call is what makes
// an unpermitted tool something the model never learns about.

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

type TokenMap struct {
	// token → user id in policy.json. Demo tokens only; a real deployment issues these per
	// user and rotates them, and this file is where the rotation would plug in.
	Tokens map[string]string `json:"tokens"`
}

type MCPFront struct {
	srv    *Server
	tokens map[string]string

	mu     sync.Mutex
	hits   int
	misses int
	bypass int // tool with ttl 0: never cached, so neither hit nor miss
	denied map[string]int
	byTool map[string]int
}

func NewMCPFront(s *Server, tokens map[string]string) *MCPFront {
	return &MCPFront{srv: s, tokens: tokens, denied: map[string]int{}, byTool: map[string]int{}}
}

func (f *MCPFront) Stats() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := f.hits + f.misses
	rate := 0.0
	if total > 0 {
		rate = float64(f.hits) / float64(total)
	}
	return map[string]any{
		"cache_hits": f.hits, "cache_misses": f.misses, "cache_bypass_no_ttl": f.bypass,
		"cache_hit_rate": rate, "calls": f.byTool, "denied": f.denied,
	}
}

func (f *MCPFront) whoami(r *http.Request) (Principal, bool) {
	h := r.Header.Get("Authorization")
	tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer"))
	if tok == "" || tok == h && !strings.HasPrefix(h, "Bearer") {
		tok = r.Header.Get("X-DSH-Token") // fallback for clients that cannot set Authorization
	}
	if tok == "" {
		return Principal{}, false
	}
	for known, userID := range f.tokens {
		if subtle.ConstantTimeCompare([]byte(known), []byte(tok)) == 1 {
			pal, err := f.srv.Policy.Resolve(userID, "dm")
			if err != nil {
				return Principal{}, false
			}
			return pal, true
		}
	}
	return Principal{}, false
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcOK(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcErr(id json.RawMessage, code int, msg string, data any) map[string]any {
	e := map[string]any{"code": code, "message": msg}
	if data != nil {
		e["data"] = data
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": e}
}

func (f *MCPFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// Streamable HTTP allows a GET SSE stream; this gateway does not offer one, and saying
		// so is better than hanging a client that expects it.
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, rpcErr(nil, -32000, "POST only", nil))
		return
	}
	pal, ok := f.whoami(r)
	if !ok {
		f.srv.audit(map[string]any{"event": "mcp_auth_failed", "path": r.URL.Path,
			"at": time.Now().UTC().Format(time.RFC3339)})
		writeJSON(w, http.StatusUnauthorized, rpcErr(nil, -32001, "unauthorized", nil))
		return
	}

	var req mcpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcErr(nil, -32700, "parse error", nil))
		return
	}
	if req.ID == nil { // notification: no response body, but still no error
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		var initParams struct {
			ClientInfo map[string]any `json:"clientInfo"`
			Protocol   string         `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &initParams)
		f.srv.audit(map[string]any{"event": "mcp_initialize", "user": pal.UserID, "role": pal.Role,
			"client": initParams.ClientInfo, "protocol": initParams.Protocol,
			"at":     time.Now().UTC().Format(time.RFC3339)})
		writeJSON(w, http.StatusOK, rpcOK(req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"serverInfo":      map[string]any{"name": "agent-gateway", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"instructions":    "Tools are filtered by the caller's role. A tool you cannot see is a tool you may not call.",
		}))
	case "ping":
		writeJSON(w, http.StatusOK, rpcOK(req.ID, map[string]any{}))
	case "tools/list":
		tools := []map[string]any{}
		for _, name := range f.srv.Policy.VisibleTools(pal, f.srv.Reg.Meta, f.srv.Reg, f.srv.Consent) {
			desc, schema, ok := f.srv.Reg.MCPToolDef(name)
			if !ok {
				continue
			}
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"name": name, "description": desc, "inputSchema": schema})
		}
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t["name"].(string))
		}
		// "Who asked to see what" is part of the audit, not a debug line: it is the only record
		// that a tool existed for a role at a moment in time.
		f.srv.audit(map[string]any{"event": "mcp_tools_list", "user": pal.UserID, "role": pal.Role,
			"count": len(names), "tools": names, "at": time.Now().UTC().Format(time.RFC3339)})
		writeJSON(w, http.StatusOK, rpcOK(req.ID, map[string]any{"tools": tools}))
	case "tools/call":
		f.call(w, r, req, pal)
	default:
		writeJSON(w, http.StatusOK, rpcErr(req.ID, -32601, "method not found: "+req.Method, nil))
	}
}

func (f *MCPFront) call(w http.ResponseWriter, r *http.Request, req mcpRequest, pal Principal) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		writeJSON(w, http.StatusOK, rpcErr(req.ID, -32602, "invalid params", nil))
		return
	}
	meta, known := f.srv.Reg.Meta[params.Name]
	decision, err := f.srv.Policy.Decide(pal, params.Name, meta, known)
	if err != nil {
		f.bumpDenied(params.Name)
		f.srv.audit(map[string]any{"event": "mcp_denied", "user": pal.UserID, "role": pal.Role,
			"tool": params.Name, "reason": err.Error(), "at": time.Now().UTC().Format(time.RFC3339)})
		writeJSON(w, http.StatusOK, rpcErr(req.ID, -32003, "denied: "+err.Error(), nil))
		return
	}

	key := hashKey(map[string]any{"tool": params.Name, "args": params.Arguments, "scope": pal.Role})
	cacheable := meta.TTL > 0
	if cacheable {
		if e, hit := f.srv.Cache.GetExact(key); hit {
			f.mu.Lock()
			f.hits++
			f.byTool[params.Name]++
			f.mu.Unlock()
			f.srv.audit(map[string]any{"event": "mcp_call", "user": pal.UserID, "role": pal.Role,
				"tool": params.Name, "cache": "hit", "at": time.Now().UTC().Format(time.RFC3339)})
			writeJSON(w, http.StatusOK, rpcOK(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": e.Answer}}}))
			return
		}
		f.mu.Lock()
		f.misses++
		f.mu.Unlock()
	} else {
		f.mu.Lock()
		f.bypass++
		f.mu.Unlock()
	}

	started := time.Now()
	res, callErr := f.srv.Reg.CallTool(params.Name, params.Arguments, pal.UserID)
	f.mu.Lock()
	f.byTool[params.Name]++
	f.mu.Unlock()

	rec := map[string]any{"event": "mcp_call", "user": pal.UserID, "role": pal.Role,
		"tool": params.Name, "decision": decision, "ms": time.Since(started).Milliseconds(),
		"cache": map[bool]string{true: "miss", false: "bypass"}[cacheable],
		"at":    time.Now().UTC().Format(time.RFC3339)}
	if callErr != nil {
		rec["error"] = callErr.Error()
	}
	f.srv.audit(rec)
	if callErr != nil {
		writeJSON(w, http.StatusOK, rpcErr(req.ID, -32000, callErr.Error(), nil))
		return
	}

	raw, _ := json.Marshal(res)
	if cacheable {
		f.srv.Cache.PutExact(key, CacheEntry{Answer: string(raw), Saved: 1,
			Created: time.Now(), TTL: time.Duration(meta.TTL) * time.Second})
	}
	writeJSON(w, http.StatusOK, rpcOK(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(raw)}}}))
}

func (f *MCPFront) bumpDenied(tool string) {
	f.mu.Lock()
	f.denied[tool]++
	f.mu.Unlock()
}
