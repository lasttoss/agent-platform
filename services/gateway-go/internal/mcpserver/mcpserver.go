// Package mcpserver is the smallest honest MCP server: JSON-RPC 2.0 over HTTP, the three
// methods a gateway actually needs (initialize, tools/list, tools/call) and nothing else.
// Tool results are returned as one text block containing a JSON object, which is what both
// this repo's Go demo and its Python services emit — a convention, stated in the README.
package mcpserver

import (
	"encoding/json"
	"net/http"
)

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type Handler struct {
	Name    string
	Version string
	Tools   []Tool
	// Call runs a tool. The gateway has already checked policy: a tool that is not in Tools
	// can never be reached, because it is not advertised.
	// Call runs a tool. The request is passed through because caller identity travels in
	// transport headers (X-Caller-Id), never as a tool argument the model could choose.
	Call func(name string, args map[string]any, r *http.Request) (any, error)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// /health is the probe the gateway uses before it re-reads a tool list. It answers one
	// question — is this process serving — and says nothing about whether the tools work.
	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": h.Name})
		return
	}
	if r.Method != http.MethodPost {
		write(w, map[string]any{"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32000, "message": "POST only"}})
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		write(w, map[string]any{"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32700, "message": "parse error"}})
		return
	}
	switch req.Method {
	case "initialize":
		write(w, result(req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]any{"name": h.Name, "version": h.Version},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}))
	case "notifications/initialized", "ping":
		write(w, result(req.ID, map[string]any{}))
	case "tools/list":
		tools := h.Tools
		if tools == nil {
			tools = []Tool{}
		}
		write(w, result(req.ID, map[string]any{"tools": tools}))
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			write(w, result(req.ID, map[string]any{"isError": true,
				"content": text("bad params: " + err.Error())}))
			return
		}
		known := false
		for _, t := range h.Tools {
			if t.Name == p.Name {
				known = true
				break
			}
		}
		if !known || h.Call == nil {
			write(w, result(req.ID, map[string]any{"isError": true,
				"content": text("unknown tool: " + p.Name)}))
			return
		}
		out, err := h.Call(p.Name, p.Arguments, r)
		if err != nil {
			write(w, result(req.ID, map[string]any{"isError": true, "content": text(err.Error())}))
			return
		}
		raw, _ := json.Marshal(out)
		write(w, result(req.ID, map[string]any{"isError": false,
			"content": text(string(raw))}))
	default:
		write(w, map[string]any{"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32601, "message": "method not found"}})
	}
}

func text(s string) []map[string]any {
	return []map[string]any{{"type": "text", "text": s}}
}

func result(id json.RawMessage, v any) map[string]any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": v}
}

func write(w http.ResponseWriter, body any) {
	raw, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
}
