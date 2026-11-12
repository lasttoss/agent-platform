package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lasttoss/agent-platform/services/gateway-go/internal/demotools"
	"github.com/lasttoss/agent-platform/services/gateway-go/internal/mcpserver"
)

const (
	policyPath  = "../gateway/config/policy.json"
	consentPath = "../../config/consent.json"
	toolsPath   = "../../config/tools.json"
)

// harness = a gateway pointed at real MCP servers over real HTTP (httptest), not at mocks of
// my own functions. If the MCP client is wrong, these tests fail.
type harness struct {
	*Server
	demo  *httptest.Server
	model *httptest.Server

	mu       sync.Mutex
	toolCall []string       // tool names the demo service was asked to run
	callerID []string       // X-Caller-Id seen by the service
	offered  [][]string     // tool names the model service was offered
	replies  map[string]any // what the fake model answers
	extra    []ServerConf   // additional MCP servers for one test
	spies    []*httptest.Server
}

type harnessOpt func(*harness)

func withModel(reply map[string]any) harnessOpt {
	return func(h *harness) { h.replies = reply }
}

func withExtraServer(name string, hand *mcpserver.Handler) harnessOpt {
	return func(h *harness) {
		srv := httptest.NewServer(hand)
		h.spies = append(h.spies, srv)
		h.extra = append(h.extra, ServerConf{Name: name, URL: srv.URL + "/mcp"})
	}
}

func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	h := &harness{replies: map[string]any{}}

	demo := &mcpserver.Handler{Name: "demo", Call: func(name string, args map[string]any, r *http.Request) (any, error) {
		h.mu.Lock()
		h.toolCall = append(h.toolCall, name)
		h.callerID = append(h.callerID, r.Header.Get("X-Caller-Id"))
		h.mu.Unlock()
		return demotools.Call(name, args, r)
	}, Tools: demotools.Handler().Tools}
	h.demo = httptest.NewServer(demo)

	for _, opt := range opts {
		opt(h)
	}
	if h.replies != nil {
		model := &mcpserver.Handler{Name: "model", Call: func(name string, args map[string]any, r *http.Request) (any, error) {
			names := []string{}
			if list, ok := args["tools"].([]any); ok {
				for _, t := range list {
					m, _ := t.(map[string]any)
					fn, _ := m["function"].(map[string]any)
					names = append(names, asString(fn["name"]))
				}
			}
			h.mu.Lock()
			h.offered = append(h.offered, names)
			h.mu.Unlock()
			out := map[string]any{"content": "", "tool_calls": []any{}}
			for k, v := range h.replies {
				out[k] = v
			}
			return out, nil
		}, Tools: []mcpserver.Tool{{Name: "chat_complete", InputSchema: map[string]any{"type": "object"}}}}
		h.model = httptest.NewServer(model)
	}
	t.Cleanup(func() {
		h.demo.Close()
		if h.model != nil {
			h.model.Close()
		}
		for _, s := range h.spies {
			s.Close()
		}
	})

	pol, err := LoadPolicy(policyPath)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	consent, err := LoadConsent(consentPath)
	if err != nil {
		t.Fatalf("consent: %v", err)
	}
	meta, err := LoadToolsMeta(toolsPath)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	confs := append([]ServerConf{{Name: "demo", URL: h.demo.URL + "/mcp",
		Tools: []string{"salary_self", "docs_search", "status"}}}, h.extra...)
	if h.model != nil {
		confs = append(confs, ServerConf{Name: "model", URL: h.model.URL + "/mcp", Role: "model"})
	}
	reg := NewRegistry(meta, confs)
	for _, err := range reg.Discover() {
		t.Fatalf("discovery: %v", err)
	}
	h.Server = NewServer(pol, consent, reg, filepath.Join(t.TempDir(), "audit.log"))
	return h
}

func (h *harness) lastToolCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.toolCall...)
}

func (h *harness) ok(t *testing.T, user, chat, text string) map[string]any {
	t.Helper()
	code, body := h.Handle(respondRequest{PlatformUserID: user, ChatType: chat, Text: text})
	if code != 200 {
		t.Fatalf("expected 200, got %d: %v", code, body)
	}
	return body
}

func TestUnknownUserIsRejected(t *testing.T) {
	h := newHarness(t)
	code, body := h.Handle(respondRequest{PlatformUserID: "9999", ChatType: "dm", Text: "lương"})
	if code != 403 || body["denied"].(map[string]any)["code"] != "unknown_user" {
		t.Fatalf("expected 403 unknown_user, got %d %v", code, body)
	}
}

func TestDMGetsPersonalAnswerOverMCP(t *testing.T) {
	h := newHarness(t)
	body := h.ok(t, "1001", "dm", "lương tháng này của tôi")
	if body["chat_reply"] != "Lương tháng này của bạn là 12.345.000 VND." {
		t.Fatalf("unexpected reply: %v", body["chat_reply"])
	}
	if calls := h.lastToolCalls(); len(calls) != 1 || calls[0] != "salary_self" {
		t.Fatalf("the answer did not come from one MCP call: %v", calls)
	}
}

// The service is told who is asking through the transport, never through an argument: a model
// that could pass a user id could ask for someone else's salary.
func TestCallerIdentityTravelsInTransportNotArguments(t *testing.T) {
	h := newHarness(t)
	h.ok(t, "2002", "dm", "lương tháng này của tôi")
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.callerID) != 1 || h.callerID[0] != "2002" {
		t.Fatalf("X-Caller-Id was %v", h.callerID)
	}
}

func TestPersonalAnswerInGroupGoesToDM(t *testing.T) {
	h := newHarness(t)
	body := h.ok(t, "1001", "group_internal", "lương tháng này của tôi")
	if body["delivered"] != "dm" || body["chat_reply"] != "Mình gửi riêng cho bạn nhé." {
		t.Fatalf("a group must not carry the number: %v", body)
	}
	if body["dm"].(map[string]any)["text"] != "Lương tháng này của bạn là 12.345.000 VND." {
		t.Fatalf("dm text wrong: %v", body["dm"])
	}
}

func TestPublicGroupHasNoPersonalTool(t *testing.T) {
	h := newHarness(t)
	body := h.ok(t, "1001", "group_public", "lương tháng này của tôi")
	visible := body["tools_visible"].([]string)
	if len(visible) != 1 || visible[0] != "docs_search" {
		t.Fatalf("public group must see only docs_search, got %v", visible)
	}
	if calls := h.lastToolCalls(); len(calls) != 0 {
		t.Fatalf("a public group reached a tool: %v", calls)
	}
}

func TestNoDiacriticsIsUnderstood(t *testing.T) {
	h := newHarness(t)
	if got := h.ok(t, "1001", "dm", "luong thang nay cua toi")["chat_reply"]; got != "Lương tháng này của bạn là 12.345.000 VND." {
		t.Fatalf("folded input not understood: %v", got)
	}
	if got := h.ok(t, "1001", "dm", "tai lieu runbook")["chat_reply"]; got != "Tài liệu liên quan: runbook-restart" {
		t.Fatalf("folded docs query wrong: %v", got)
	}
}

// A server may advertise anything; the policy file still decides. deploy_prod is advertised
// here on purpose, and must stay invisible and unreachable.
func TestDestructiveToolIsNeverOfferedEvenWhenAServerAdvertisesIt(t *testing.T) {
	h := newHarness(t, withExtraServer("rogue", &mcpserver.Handler{
		Name:  "rogue",
		Tools: []mcpserver.Tool{{Name: "deploy_prod", InputSchema: map[string]any{"type": "object"}}},
		Call: func(string, map[string]any, *http.Request) (any, error) {
			t.Fatal("deploy_prod was executed")
			return nil, nil
		}}))
	if !h.Reg.Advertises("deploy_prod") {
		t.Fatal("test setup: the rogue server should advertise deploy_prod")
	}
	for _, chat := range []string{"dm", "group_internal", "group_public"} {
		pal, err := h.Policy.Resolve("1001", chat)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range append(h.Policy.VisibleTools(pal, h.Reg.Meta, h.Reg, h.Consent),
			h.Policy.RoutableTools(pal, h.Reg.Meta, h.Reg, h.Consent)...) {
			if name == "deploy_prod" {
				t.Fatalf("deploy_prod reachable in %s", chat)
			}
		}
	}
	if _, err := h.Policy.Decide(pal(t, h, "dm"), "deploy_prod", h.Reg.Meta["deploy_prod"], true); err == nil {
		t.Fatal("Decide allowed a destructive tool")
	}
}

// Consent is only meaningful for a tool that exists: filtering happens before the model and
// before the registry lookup, so an ungranted provider tool is absent, not denied later.
func TestConsentGatesAnAdvertisedProviderTool(t *testing.T) {
	h := newHarness(t, withExtraServer("drive", &mcpserver.Handler{
		Name:  "drive",
		Tools: []mcpserver.Tool{{Name: "drive_search", InputSchema: map[string]any{"type": "object"}}},
		Call: func(string, map[string]any, *http.Request) (any, error) {
			return map[string]any{"files": []any{}}, nil
		}}))
	pal, err := h.Policy.Resolve("1001", "dm")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(h.Policy.VisibleTools(pal, h.Reg.Meta, h.Reg, h.Consent), "drive_search") {
		t.Fatal("a consented user should see an advertised provider tool")
	}
	other, err := h.Policy.Resolve("2002", "dm")
	if err != nil {
		t.Fatal(err)
	}
	if contains(h.Policy.VisibleTools(other, h.Reg.Meta, h.Reg, h.Consent), "drive_search") {
		t.Fatal("an unconsented user must not see it")
	}
}

// A tool nobody serves does not exist, even if the policy names it: that is the difference
// between "configured" and "running", and it is the honest answer to a caller.
func TestAToolWithNoRunningServerIsInvisible(t *testing.T) {
	h := newHarness(t)
	pal, _ := h.Policy.Resolve("1001", "dm")
	visible := h.Policy.VisibleTools(pal, h.Reg.Meta, h.Reg, h.Consent)
	for _, missing := range []string{"jira_search", "mail_list", "drive_search", "ocr_extract"} {
		if contains(visible, missing) {
			t.Fatalf("%s is not served by any running service but was offered: %v", missing, visible)
		}
	}
	if body := h.ok(t, "1001", "dm", "mail của tôi"); body["chat_reply"] != "Mình chưa có công cụ cho việc này." {
		t.Fatalf("expected an honest refusal, got %v", body["chat_reply"])
	}
}

func TestSecondIdenticalQuestionComesFromCache(t *testing.T) {
	h := newHarness(t)
	h.ok(t, "1001", "dm", "tài liệu runbook")
	body := h.ok(t, "1001", "dm", "tài liệu runbook")
	cache, cached := body["cache"].(map[string]any)
	if !cached || cache["layer"] != "exact" {
		t.Fatalf("expected an exact cache hit, got %v", body["cache"])
	}
	if calls := h.lastToolCalls(); len(calls) != 1 {
		t.Fatalf("the cache hit still called the service: %v", calls)
	}
}

func TestSalaryIsNeverCached(t *testing.T) {
	h := newHarness(t)
	h.ok(t, "1001", "dm", "lương tháng này của tôi")
	body := h.ok(t, "1001", "dm", "lương tháng này của tôi")
	if _, cached := body["cache"]; cached {
		t.Fatalf("a salary answer was cached: %v", body)
	}
}

func TestCacheDoesNotLeakBetweenUsers(t *testing.T) {
	h := newHarness(t)
	h.ok(t, "1001", "dm", "lương tháng này của tôi")
	if got := h.ok(t, "2002", "dm", "lương tháng này của tôi")["chat_reply"]; got != "Lương tháng này của bạn là 9.876.000 VND." {
		t.Fatalf("another user's salary leaked: %v", got)
	}
}

func TestQuotaIsEnforced(t *testing.T) {
	h := newHarness(t)
	h.Policy.Quota.RequestsPerDay = 1
	h.ok(t, "1001", "dm", "trạng thái")
	code, body := h.Handle(respondRequest{PlatformUserID: "1001", ChatType: "dm", Text: "trạng thái"})
	if code != 429 {
		t.Fatalf("expected 429, got %d: %v", code, body)
	}
}

func TestNoToolQuestionIsRefusedWithoutAModel(t *testing.T) {
	h := newHarness(t)
	if got := h.ok(t, "1001", "dm", "thời tiết hôm nay thế nào")["chat_reply"]; got != "Mình chưa có công cụ cho việc này." {
		t.Fatalf("ungrounded answer without a model: %v", got)
	}
}

// The model is a service too, and it is offered exactly the tools the caller may use.
func TestTheModelIsOfferedOnlyVisibleTools(t *testing.T) {
	h := newHarness(t, withModel(map[string]any{"content": ""}))
	h.ok(t, "1001", "dm", "thời tiết hôm nay thế nào")
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.offered) != 1 {
		t.Fatalf("the model service was called %d times", len(h.offered))
	}
	for _, name := range h.offered[0] {
		if name == "deploy_prod" || name == "mail_list" {
			t.Fatalf("the model was told about %s", name)
		}
	}
	if !contains(h.offered[0], "docs_search") {
		t.Fatalf("the model should have been offered docs_search: %v", h.offered[0])
	}
}

func TestAModelChosenToolActuallyRuns(t *testing.T) {
	h := newHarness(t, withModel(map[string]any{"content": "",
		"tool_calls": []any{map[string]any{"name": "docs_search",
			"arguments": map[string]any{"q": "runbook"}}}}))
	body := h.ok(t, "1001", "dm", "tìm giúp tôi cái gì đó")
	if body["chat_reply"] != "Tài liệu liên quan: runbook-restart" {
		t.Fatalf("model-chosen tool did not run: %v", body)
	}
	if calls := h.lastToolCalls(); len(calls) != 1 || calls[0] != "docs_search" {
		t.Fatalf("tool call trace: %v", calls)
	}
}

// The dangerous case: the model asks for a tool it was never offered. Nothing executes.
func TestAToolTheModelWasNotOfferedIsNotExecuted(t *testing.T) {
	h := newHarness(t, withModel(map[string]any{"content": "",
		"tool_calls": []any{map[string]any{"name": "deploy_prod", "arguments": map[string]any{}}}}))
	body := h.ok(t, "1001", "dm", "deploy giúp tôi")
	if body["denied_tool"] != "deploy_prod" {
		t.Fatalf("expected denial, got %v", body)
	}
	if calls := h.lastToolCalls(); len(calls) != 0 {
		t.Fatalf("a denied tool still reached a service: %v", calls)
	}
}

func TestFreeChatOnlyWherePolicyAllowsIt(t *testing.T) {
	h := newHarness(t, withModel(map[string]any{"content": "MODEL-ANSWER"}))
	if got := h.ok(t, "1001", "dm", "chào bạn")["chat_reply"]; got != "MODEL-ANSWER" {
		t.Fatalf("dm free_chat should deliver the model's text: %v", got)
	}
	body := h.ok(t, "1001", "group_public", "chào bạn")
	if body["chat_reply"] == "MODEL-ANSWER" {
		t.Fatal("free_chat leaked into a public group")
	}
}

func TestFoldAndKeys(t *testing.T) {
	if Fold("LƯƠNG tháng này") != Fold("luong thang nay") {
		t.Fatalf("fold mismatch: %q vs %q", Fold("LƯƠNG tháng này"), Fold("luong thang nay"))
	}
	a := ExactKey("lương", []string{"a", "b"}, "v1", "dm", "user:1")
	b := ExactKey("luong", []string{"b", "a"}, "v1", "dm", "user:1")
	if a != b {
		t.Fatal("tool order or diacritics changed the cache key")
	}
	if a == ExactKey("lương", []string{"a", "b"}, "v1", "dm", "user:2") {
		t.Fatal("scope must be part of the cache key")
	}
	if PrefixFingerprint([]string{"a"}, "v1") == PrefixFingerprint([]string{"a"}, "v2") {
		t.Fatal("prefix fingerprint must change with the policy version")
	}
}

func TestLocalOverrideKeepsRealValuesOutOfGit(t *testing.T) {
	pol, err := LoadPolicy(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	consent, _ := LoadConsent(consentPath)
	path := filepath.Join(t.TempDir(), "local.json")
	os.WriteFile(path, []byte(`{"users":{"424242":{"name":"Local","role":"owner","workspace":"x"}}}`), 0o644)
	local, err := LoadLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	ApplyLocal(pol, consent, local)
	if _, err := pol.Resolve("424242", "dm"); err != nil {
		t.Fatalf("local user should resolve: %v", err)
	}
}

func TestMCPClientRejectsAnUnknownTool(t *testing.T) {
	h := newHarness(t)
	if _, err := h.Reg.CallTool("mail_list", nil, "1001"); err == nil {
		t.Fatal("calling a tool nobody advertises must fail, not return empty data")
	}
}

func TestAuditLineRecordsTheDecision(t *testing.T) {
	h := newHarness(t)
	h.ok(t, "1001", "group_internal", "lương tháng này của tôi")
	raw, err := os.ReadFile(h.Audit)
	if err != nil {
		t.Fatalf("no audit log: %v", err)
	}
	var rec map[string]any
	for _, line := range splitLines(string(raw)) {
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("audit line is not JSON: %s", line)
		}
	}
	if rec["decision"] != "private_delivery" || rec["tool"] != "salary_self" {
		t.Fatalf("audit does not state what happened: %v", rec)
	}
}

func pal(t *testing.T, h *harness, chat string) Principal {
	t.Helper()
	p, err := h.Policy.Resolve("1001", chat)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func splitLines(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}
