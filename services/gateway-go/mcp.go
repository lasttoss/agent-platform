package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"
)

// Everything except the gateway is a service behind MCP. The gateway keeps policy, quota,
// cache and audit; it does not know how to extract a PDF, read mail, or talk to a model.
// Adding a capability means adding a server — not a new branch in the gateway.
type ServerConf struct {
	Name      string   `json:"name"`
	URL       string   `json:"url"`
	HealthURL string   `json:"health_url"` // default: same host as URL, path /health
	TokenEnv  string   `json:"token_env"`
	Role      string   `json:"role"` // "" = tool service, "model" = the brain
	Tools     []string `json:"tools"`
}

type ToolMeta struct {
	Personal    bool   `json:"personal"`
	Destructive bool   `json:"destructive"`
	Provider    string `json:"provider"`
	Scope       string `json:"scope"`
	TTL         int    `json:"ttl"`           // seconds; 0 = never cache
	MaxInFlight int    `json:"max_in_flight"` // 0 = unlimited (see README: that is a choice)
	// TimeoutS is how long this tool may take. A build or an import needs minutes; a search
	// needs seconds, and one global timeout is either too short for the first or too long for
	// the second. 0 keeps the historical 60s.
	TimeoutS float64 `json:"timeout_s"`
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
	return c.rpcAsTimeout(method, params, caller, 60*time.Second)
}

// rpcAsTimeout is rpcAs with the tool's own deadline: a search gets seconds, an import gets
// minutes, and one global timeout would be wrong for one of them.
func (c *Client) rpcAsTimeout(method string, params any, caller string, timeout time.Duration) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
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
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
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
	return c.CallTimeout(name, args, caller, 60*time.Second)
}

// CallTimeout is Call with a deadline chosen per tool (see ToolMeta.TimeoutS).
func (c *Client) CallTimeout(name string, args map[string]any, caller string, timeout time.Duration) (map[string]any, error) {
	raw, err := c.rpcAsTimeout("tools/call", map[string]any{"name": name, "arguments": args}, caller, timeout)
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
// A tool that no reachable server advertises does not exist, so it can never be offered to a
// model or called by a chat.
//
// Reachability is re-checked on a timer (30 s by default): health first, then the MCP tool list.
// A service that was down at boot and came up afterwards becomes visible without restarting the
// gateway — the alternative is a deploy order that somebody has to remember.
type Registry struct {
	Meta    map[string]ToolMeta
	Servers []*MCPServer
	byTool  map[string]*MCPServer
	Model   *MCPServer

	mu     sync.RWMutex
	health *http.Client
}

// RefreshReport says what changed, so the caller can log changes and stay quiet about no-ops.
type RefreshReport struct {
	Added   []string
	Removed []string
	Errors  []error
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
	r := &Registry{Meta: meta, byTool: map[string]*MCPServer{},
		health: &http.Client{Timeout: 2 * time.Second}}
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

// listTools reads a server's allowlisted tool list. The config is the allowlist: a tool a
// running service advertises but the gateway's config does not name is not a capability.
func (r *Registry) listTools(srv *MCPServer) (map[string]MCPTool, error) {
	tools, err := srv.Client.ListTools()
	if err != nil {
		return nil, err
	}
	out := map[string]MCPTool{}
	for _, t := range tools {
		if len(srv.Conf.Tools) > 0 && !contains(srv.Conf.Tools, t.Name) {
			continue
		}
		out[t.Name] = t
	}
	return out, nil
}

// healthURL is the probe address: explicit when configured, otherwise the same host as the MCP
// endpoint with the path replaced, which is what every service in this repo serves.
func healthURL(conf ServerConf) string {
	if conf.HealthURL != "" {
		return conf.HealthURL
	}
	u, err := url.Parse(conf.URL)
	if err != nil || u.Host == "" {
		return ""
	}
	u.Path, u.RawQuery, u.Fragment = "/health", "", ""
	return u.String()
}

// probe answers three things: is it up, does it even have a health endpoint, and why.
// A 404 means "no health endpoint", not "broken": treating the absence of an optional endpoint
// as an outage would delete a working service's tools on every tick.
func (r *Registry) probe(conf ServerConf) (healthy bool, known bool, detail string) {
	target := healthURL(conf)
	if target == "" {
		return true, false, "no health url"
	}
	resp, err := r.health.Get(target)
	if err != nil {
		return false, true, err.Error()
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, true, "200"
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return true, false, "no health endpoint"
	}
	return false, true, resp.Status
}

// Refresh probes every service, then re-reads the tool lists of the ones that answer. A failing
// probe removes that service's tools immediately: offering a tool whose service is down turns a
// fast 503 into a slow timeout. A failing tool list keeps the previous list, because a single
// bad read should not empty the catalogue.
func (r *Registry) Refresh() RefreshReport {
	var rep RefreshReport
	servers := append([]*MCPServer{}, r.Servers...)
	for _, srv := range servers {
		healthy, _, detail := r.probe(srv.Conf)
		if !healthy {
			r.mu.Lock()
			if srv.Conf.Role == "model" {
				if r.Model != nil {
					rep.Removed = append(rep.Removed, "model")
					r.Model = nil
				}
			} else {
				for name := range srv.Tools {
					delete(r.byTool, name)
					rep.Removed = append(rep.Removed, name)
				}
				srv.Tools = map[string]MCPTool{}
			}
			r.mu.Unlock()
			rep.Errors = append(rep.Errors, fmt.Errorf("%s: health %s", srv.Conf.Name, detail))
			continue
		}

		if srv.Conf.Role == "model" {
			if r.Model == nil {
				r.mu.Lock()
				r.Model = srv
				r.mu.Unlock()
				rep.Added = append(rep.Added, "model")
			}
			continue
		}

		tools, err := r.listTools(srv)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("%s: tools/list: %w", srv.Conf.Name, err))
			continue
		}
		r.mu.Lock()
		for name := range srv.Tools {
			if _, still := tools[name]; !still {
				delete(r.byTool, name)
				rep.Removed = append(rep.Removed, name)
			}
		}
		for name, t := range tools {
			if _, had := srv.Tools[name]; !had {
				rep.Added = append(rep.Added, name)
			}
			srv.Tools[name] = t
			r.byTool[name] = srv
		}
		r.mu.Unlock()
	}
	sort.Strings(rep.Added)
	sort.Strings(rep.Removed)
	return rep
}

// Discover is Refresh: one code path, so the boot state and the 30-second state cannot drift.
func (r *Registry) Discover() []error { return r.Refresh().Errors }

func (r *Registry) Advertises(tool string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byTool[tool]
	return ok
}

func (r *Registry) Schema(tool string) map[string]any {
	r.mu.RLock()
	srv, ok := r.byTool[tool]
	if !ok {
		r.mu.RUnlock()
		return nil
	}
	t := srv.Tools[tool]
	r.mu.RUnlock()
	desc := t.Description
	if m, ok := r.Meta[tool]; ok && m.Description != "" {
		desc = m.Description
	}
	return map[string]any{"type": "function", "function": map[string]any{
		"name": tool, "description": desc, "parameters": t.InputSchema}}
}

func (r *Registry) CallTool(tool string, args map[string]any, caller string) (map[string]any, error) {
	r.mu.RLock()
	srv, ok := r.byTool[tool]
	meta := r.Meta[tool]
	r.mu.RUnlock()
	if !ok {
		return nil, errors.New("no server advertises " + tool)
	}
	timeout := 60 * time.Second
	if meta.TimeoutS > 0 {
		timeout = time.Duration(meta.TimeoutS * float64(time.Second))
	}
	return srv.Client.CallTimeout(tool, args, caller, timeout)
}

// Complete asks the model service — an MCP server, not a hardcoded provider — to choose a tool.
// The gateway sends only the schemas it has already filtered, so an unoffered tool is not a
// request the model can make.
func (r *Registry) Complete(system, user string, tools []map[string]any) (ModelTurn, error) {
	r.mu.RLock()
	model := r.Model
	r.mu.RUnlock()
	if model == nil {
		return ModelTurn{}, errors.New("no model service configured")
	}
	out, err := model.Client.Call("chat_complete", map[string]any{
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

// MCPToolDef exposes one advertised tool in MCP shape (description + inputSchema). The OpenAI
// shape returned by Schema() is for the model service; an MCP client needs the original.
func (r *Registry) MCPToolDef(tool string) (string, map[string]any, bool) {
	r.mu.RLock()
	srv, ok := r.byTool[tool]
	if !ok {
		r.mu.RUnlock()
		return "", nil, false
	}
	t := srv.Tools[tool]
	r.mu.RUnlock()
	desc := t.Description
	if m, ok := r.Meta[tool]; ok {
		if m.Description != "" {
			desc = m.Description
		}
		if m.Provider != "" {
			desc += " (provider: " + m.Provider + ")"
		}
	}
	return desc, t.InputSchema, true
}
