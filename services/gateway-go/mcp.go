package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// Everything except the gateway is a service behind MCP. The gateway keeps policy, quota,
// cache and audit; it does not know how to extract a PDF, read mail, or talk to a model.
// Adding a capability means adding a server — not a new branch in the gateway.
type ServerConf struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	TokenEnv string   `json:"token_env"`
	Role     string   `json:"role"` // "" = tool service, "model" = the brain
	Tools    []string `json:"tools"`
}

type ToolMeta struct {
	Personal    bool   `json:"personal"`
	Destructive bool   `json:"destructive"`
	Provider    string `json:"provider"`
	Scope       string `json:"scope"`
	TTL         int    `json:"ttl"` // seconds; 0 = never cache
	Description string `json:"description"`
}

func LoadToolsMeta(path string) (map[string]ToolMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]ToolMeta{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("tools metadata is empty: every tool would be invisible")
	}
	return out, nil
}

func LoadServers(path string) ([]ServerConf, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Servers []ServerConf `json:"servers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc.Servers, nil
}

type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Client is one MCP endpoint. Stateless per call: no session id is required by the services
// in this repo, and pretending to hold one would hide that fact.
type Client struct {
	URL   string
	Token string

	mu sync.Mutex
	id int
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) rpc(method string, params any) (json.RawMessage, error) {
	return c.rpcAs(method, params, "")
}

// rpcAs is the same call, plus who is on whose behalf. The gateway has already resolved the
// principal; the service trusts this header only because it is reachable only from the gateway.
func (c *Client) rpcAs(method string, params any, caller string) (json.RawMessage, error) {
	c.mu.Lock()
	c.id++
	id := c.id
	c.mu.Unlock()

	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id,
		"method": method, "params": params})
	req, err := http.NewRequest(http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if caller != "" {
		req.Header.Set("X-Caller-Id", caller)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", method, err)
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("%s: %s (%d)", method, envelope.Error.Message, envelope.Error.Code)
	}
	return envelope.Result, nil
}

func (c *Client) ListTools() ([]MCPTool, error) {
	raw, err := c.rpc("tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc.Tools, nil
}

// Call returns the tool's JSON object. MCP wraps results in a content list; this repo's
// services put one JSON document in a single text block, so there is one shape to parse.
func (c *Client) Call(name string, args map[string]any, caller string) (map[string]any, error) {
	raw, err := c.rpcAs("tools/call", map[string]any{"name": name, "arguments": args}, caller)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("tool returned no content")
	}
	text := doc.Content[0].Text
	if doc.IsError {
		return nil, errors.New(text)
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return map[string]any{"text": text}, nil
	}
	return out, nil
}

// Registry is the list of services this gateway may talk to, and what each one said it can do.
// Discovery happens once at boot: a tool that no server advertises does not exist, so it can
// never be offered to a model or called by a chat.
type Registry struct {
	Meta    map[string]ToolMeta
	Servers []*MCPServer
	byTool  map[string]*MCPServer
	Model   *MCPServer
}

type MCPServer struct {
	Conf   ServerConf
	Client *Client
	Tools  map[string]MCPTool
}

// NewRegistry records the services. It does not talk to them yet: discovery is an explicit
// step whose failures are printed, instead of a constructor that quietly drops a service —
// silence there would look exactly like "this tool was never configured".
func NewRegistry(meta map[string]ToolMeta, confs []ServerConf) *Registry {
	r := &Registry{Meta: meta, byTool: map[string]*MCPServer{}}
	for _, conf := range confs {
		token := ""
		if conf.TokenEnv != "" {
			token = os.Getenv(conf.TokenEnv)
		}
		srv := &MCPServer{Conf: conf, Client: &Client{URL: conf.URL, Token: token},
			Tools: map[string]MCPTool{}}
		r.Servers = append(r.Servers, srv)
		if conf.Role == "model" {
			r.Model = srv
		}
	}
	return r
}

func (r *Registry) add(srv *MCPServer) error {
	tools, err := srv.Client.ListTools()
	if err != nil {
		return err
	}
	for _, t := range tools {
		// A server may advertise more than the gateway's config allows: the config is the
		// allowlist, an unreviewed tool on a running service is not a capability.
		if len(srv.Conf.Tools) > 0 && !contains(srv.Conf.Tools, t.Name) {
			continue
		}
		srv.Tools[t.Name] = t
		r.byTool[t.Name] = srv
	}
	return nil
}

// Discover re-reads the tool lists. Called at boot and by tests.
func (r *Registry) Discover() []error {
	var errs []error
	for _, srv := range r.Servers {
		if srv.Conf.Role == "model" {
			continue
		}
		if err := r.add(srv); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", srv.Conf.Name, err))
		}
	}
	return errs
}

func (r *Registry) Advertises(tool string) bool {
	_, ok := r.byTool[tool]
	return ok
}

func (r *Registry) Schema(tool string) map[string]any {
	srv, ok := r.byTool[tool]
	if !ok {
		return nil
	}
	t := srv.Tools[tool]
	desc := t.Description
	if m, ok := r.Meta[tool]; ok && m.Description != "" {
		desc = m.Description
	}
	return map[string]any{"type": "function", "function": map[string]any{
		"name": tool, "description": desc, "parameters": t.InputSchema}}
}

func (r *Registry) CallTool(tool string, args map[string]any, caller string) (map[string]any, error) {
	srv, ok := r.byTool[tool]
	if !ok {
		return nil, errors.New("no server advertises " + tool)
	}
	return srv.Client.Call(tool, args, caller)
}

// Complete asks the model service — an MCP server, not a hardcoded provider — to choose a tool.
// The gateway sends only the schemas it has already filtered, so an unoffered tool is not a
// request the model can make.
func (r *Registry) Complete(system, user string, tools []map[string]any) (ModelTurn, error) {
	if r.Model == nil {
		return ModelTurn{}, errors.New("no model service configured")
	}
	out, err := r.Model.Client.Call("chat_complete", map[string]any{
		"system": system, "user": user, "tools": tools}, "")
	if err != nil {
		return ModelTurn{}, err
	}
	turn := ModelTurn{Content: asString(out["content"])}
	if usage, ok := out["usage"].(map[string]any); ok {
		turn.PromptTokens = asInt(usage["prompt_tokens"])
		turn.CompletionTokens = asInt(usage["completion_tokens"])
	}
	if calls, ok := out["tool_calls"].([]any); ok {
		for _, c := range calls {
			m, _ := c.(map[string]any)
			turn.ToolCalls = append(turn.ToolCalls, ToolCall{
				Name: asString(m["name"]), Args: asMap(m["arguments"])})
		}
	}
	return turn, nil
}

func asString(v any) string { s, _ := v.(string); return s }

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
