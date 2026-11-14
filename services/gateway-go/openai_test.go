package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lasttoss/agent-platform/services/gateway-go/internal/mcpserver"
)

// The OpenAI-shaped endpoint, exercised the way a harness uses it: bearer token → whitelist →
// model service. Nothing here asserts on the provider; the model service is a stub that records
// what it was asked for.
type oaFixture struct {
	base       string
	owner      string
	dev        string
	calls      *[]map[string]any
	audit      string
	tokensPath string
	server     *Server
}

func newOA(t *testing.T) *oaFixture {
	t.Helper()
	calls := []map[string]any{}
	model := &mcpserver.Handler{
		Name: "model", Version: "t",
		Tools: []mcpserver.Tool{{Name: "chat_complete", InputSchema: map[string]any{"type": "object"}}},
		Call: func(name string, args map[string]any, r *http.Request) (any, error) {
			calls = append(calls, args)
			return map[string]any{
				"content": "echo: " + asString(args["user"]), "tool_calls": []any{},
				"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
			}, nil
		},
	}
	upstream := httptest.NewServer(model)
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	policyPath := write("policy.json", `{
      "allow_unknown_users": false,
      "users": {"1001": {"name":"Owner","role":"owner","workspace":"d"},
                "2002": {"name":"Dev","role":"dev","workspace":"d"}},
      "roles": {"owner": {"tools":["*"],"admin":true}, "dev": {"tools":["*"]}},
      "chats": {"dm": {"tools":[], "personal_data": true, "free_chat": true}},
      "quota": {"requests_per_day": 1000, "tokens_per_day": 100000},
      "models": {"allow": ["gpt-4.1-mini","gpt-4.1","only-owner"],
                 "by_role": {"dev": ["gpt-4.1-mini"]},
                 "default": "gpt-4.1-mini"}
    }`)
	consentPath := write("consent.json", `{}`)
	metaPath := write("tools.json", `{"noop":{"description":"unused here","ttl":0}}`)
	mcpPath := write("mcp.json", `{"servers":[{"name":"model","url":"`+upstream.URL+`/mcp","role":"model"}]}`)
	tokensPath := write("tokens.json", `{"tokens":{"tok-owner":"1001","tok-dev":"2002"}}`)

	pol, err := LoadPolicy(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	consent, err := LoadConsent(consentPath)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := LoadToolsMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	confs, err := LoadServers(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(meta, confs)
	for _, e := range reg.Discover() {
		t.Fatalf("discovery: %v", err2str(e))
	}
	auditPath := filepath.Join(dir, "audit.log")
	srv := NewServer(pol, consent, reg, auditPath)
	ts, err := NewTokenSet(tokensPath)
	if err != nil {
		t.Fatal(err)
	}
	srv.MCP = NewMCPFront(srv, ts)
	tsrv := httptest.NewServer(srv.Routes())
	t.Cleanup(tsrv.Close)
	return &oaFixture{base: tsrv.URL, owner: "tok-owner", dev: "tok-dev", calls: &calls,
		audit: auditPath, tokensPath: tokensPath, server: srv}
}

func err2str(e error) string {
	if e == nil {
		return "<nil>"
	}
	return e.Error()
}

func (f *oaFixture) post(t *testing.T, path, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func (f *oaFixture) models(t *testing.T, token string) ([]string, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.base+"/v1/models", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var doc struct {
		Object string                `json:"object"`
		Data   []struct{ ID string } `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unreadable list: %v (%s)", err, raw)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	if doc.Object != "list" {
		t.Fatalf("object = %q, want list", doc.Object)
	}
	out := []string{}
	for _, m := range doc.Data {
		out = append(out, m.ID)
	}
	return out, resp.StatusCode
}

func TestOpenAIModelsIsTheWhitelist(t *testing.T) {
	f := newOA(t)
	owner, status := f.models(t, f.owner)
	if status != http.StatusOK || strings.Join(owner, ",") != "gpt-4.1-mini,gpt-4.1,only-owner" {
		t.Fatalf("owner models = %v (%d)", owner, status)
	}
	// A role narrows the list; it cannot extend it.
	dev, _ := f.models(t, f.dev)
	if strings.Join(dev, ",") != "gpt-4.1-mini" {
		t.Fatalf("dev models = %v", dev)
	}
}

func TestOpenAIModelsNeedsAToken(t *testing.T) {
	f := newOA(t)
	got, status := f.models(t, "")
	if status != http.StatusUnauthorized || len(got) != 0 {
		t.Fatalf("unauthenticated list = %v (%d)", got, status)
	}
}

func TestOpenAIChatRefusesAModelOutsideTheWhitelist(t *testing.T) {
	f := newOA(t)
	status, body := f.post(t, "/v1/chat/completions", f.owner,
		`{"model":"gpt-5-expensive","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusNotFound || !strings.Contains(body, "model_not_found") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(*f.calls) != 0 {
		t.Fatalf("a refused model still reached the provider: %v", *f.calls)
	}
	// A role that may not use a model gets the same answer as one whose model does not exist:
	// otherwise the endpoint becomes a way to enumerate what other roles have.
	status, body = f.post(t, "/v1/chat/completions", f.dev,
		`{"model":"only-owner","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusNotFound || !strings.Contains(body, "model_not_found") {
		t.Fatalf("dev got status=%d body=%s", status, body)
	}
}

func TestOpenAIChatAnswersInTheShapeClientsParse(t *testing.T) {
	f := newOA(t)
	status, body := f.post(t, "/v1/chat/completions", f.dev,
		`{"model":"gpt-4.1-mini","platform_user_id":"1001","messages":[
		   {"role":"system","content":"be brief"},{"role":"user","content":"hello"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var doc struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unreadable: %v (%s)", err, body)
	}
	if !strings.HasPrefix(doc.ID, "chatcmpl-") || doc.Object != "chat.completion" || doc.Model != "gpt-4.1-mini" {
		t.Fatalf("shape: %s", body)
	}
	if len(doc.Choices) != 1 || doc.Choices[0].Message.Role != "assistant" ||
		doc.Choices[0].FinishReason != "stop" {
		t.Fatalf("choices: %s", body)
	}
	if doc.Usage.PromptTokens != 11 || doc.Usage.CompletionTokens != 7 || doc.Usage.TotalTokens != 18 {
		t.Fatalf("usage: %s", body)
	}
	// Identity is the transport's: the body named 1001, the token says 2002, and the model saw
	// the system prompt plus the user turn.
	if len(*f.calls) != 1 {
		t.Fatalf("model calls = %d", len(*f.calls))
	}
	if asString((*f.calls)[0]["system"]) != "be brief" || asString((*f.calls)[0]["user"]) != "hello" {
		t.Fatalf("forwarded args: %v", (*f.calls)[0])
	}
	if asString((*f.calls)[0]["model"]) != "gpt-4.1-mini" {
		t.Fatalf("model not forwarded: %v", (*f.calls)[0])
	}
	raw, err := os.ReadFile(f.audit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"event":"openai_chat"`) ||
		!strings.Contains(string(raw), `"user":"2002"`) {
		t.Fatalf("audit did not record the call: %s", raw)
	}
}

func TestOpenAIChatStreamsValidSSE(t *testing.T) {
	f := newOA(t)
	status, body := f.post(t, "/v1/chat/completions", f.owner,
		`{"model":"gpt-4.1-mini","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	for _, want := range []string{"chat.completion.chunk", `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in: %s", want, body)
		}
	}
}

func TestOpenAIChatNeedsAToken(t *testing.T) {
	f := newOA(t)
	status, body := f.post(t, "/v1/chat/completions", "",
		`{"model":"gpt-4.1-mini","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusUnauthorized || !strings.Contains(body, "invalid_api_key") {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(*f.calls) != 0 {
		t.Fatalf("unauthenticated call reached the provider")
	}
}
