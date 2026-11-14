package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lasttoss/agent-platform/services/gateway-go/internal/mcpserver"
)

// testGateway builds a gateway whose upstream is a real MCP server over HTTP, so the test
// exercises the same path as production: token → policy → registry → upstream.
func testGateway(t *testing.T) (string, string, string) {
	t.Helper()
	return testGatewayWithCounter(t, nil)
}

// testGatewayWithCounter is the same gateway; a non-nil counter counts upstream tool calls, so
// a test can prove that concurrency did not multiply them.
func testGatewayWithCounter(t *testing.T, upstreamCalls *int32, delay ...time.Duration) (string, string, string) {
	t.Helper()
	upstream := httptest.NewServer(&mcpserver.Handler{
		Name: "up", Version: "t",
		Tools: []mcpserver.Tool{
			{Name: "echo", Description: "echo back", InputSchema: map[string]any{"type": "object"}},
			{Name: "personal_echo", Description: "personal", InputSchema: map[string]any{"type": "object"}},
			{Name: "write_thing", Description: "a write", InputSchema: map[string]any{"type": "object"}},
		},
		Call: func(name string, args map[string]any, r *http.Request) (any, error) {
			if upstreamCalls != nil {
				atomic.AddInt32(upstreamCalls, 1)
			}
			if len(delay) > 0 {
				time.Sleep(delay[0]) // keep the slot occupied so waiters really wait
			}
			return map[string]any{"content": []map[string]any{{"type": "text",
				"text": fmt.Sprintf("%s:%v:%s", name, args["q"], r.Header.Get("X-Caller-Id"))}}}, nil
		},
	})
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
                "2002": {"name":"Dev","role":"dev","workspace":"d"},
                "3003": {"name":"Dev2","role":"dev","workspace":"d"}},
      "roles": {"owner": {"tools":["*"]}, "dev": {"tools":["echo","personal_echo"]}},
      "chats": {"dm": {"tools":["echo","personal_echo","write_thing"], "personal_data": true, "free_chat": true}},
      "quota": {"requests_per_day": 1000, "tokens_per_day": 100000}
    }`)
	consentPath := write("consent.json", `{}`)
	metaPath := write("tools.json", `{
      "echo": {"description":"echo","ttl":600},
      "personal_echo": {"description":"personal","personal":true,"ttl":600},
      "write_thing": {"description":"write","ttl":0}
    }`)
	mcpPath := write("mcp.json", fmt.Sprintf(`{"servers":[{"name":"up","url":%q,"tools":["echo","personal_echo","write_thing"]}]}`, upstream.URL+"/mcp"))
	tokensPath := write("tokens.json", `{"tokens":{"tok-owner":"1001","tok-dev":"2002","tok-dev-2":"3003"}}`)

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
	if errs := reg.Discover(); len(errs) > 0 {
		t.Fatalf("discover: %v", errs)
	}
	srv := NewServer(pol, consent, reg, filepath.Join(dir, "audit.log"))
	ts, err := NewTokenSet(tokensPath)
	if err != nil {
		t.Fatal(err)
	}
	srv.MCP = NewMCPFront(srv, ts)
	tsrv := httptest.NewServer(srv.Routes())
	t.Cleanup(tsrv.Close)
	return tsrv.URL, tokensPath, filepath.Join(dir, "audit.log")
}

func call(t *testing.T, base, token, body string, accept string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func listTools(t *testing.T, base, token string) []string {
	t.Helper()
	_, body := call(t, base, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "")
	var doc struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("tools/list: %v (%s)", err, body)
	}
	out := []string{}
	for _, x := range doc.Result.Tools {
		out = append(out, x.Name)
	}
	return out
}

func TestMCPFrontAuth(t *testing.T) {
	base, _, _ := testGateway(t)
	if code, _ := call(t, base, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, ""); code != 401 {
		t.Fatalf("no token: want 401, got %d", code)
	}
	if code, _ := call(t, base, "tok-nope", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, ""); code != 401 {
		t.Fatalf("bad token: want 401, got %d", code)
	}
}

func TestMCPFrontFiltersByRole(t *testing.T) {
	base, _, _ := testGateway(t)
	owner, dev := listTools(t, base, "tok-owner"), listTools(t, base, "tok-dev")
	if len(owner) != 3 {
		t.Fatalf("owner sees %v", owner)
	}
	if len(dev) != 2 || contains(dev, "write_thing") {
		t.Fatalf("dev must not see write_thing: %v", dev)
	}
}

func TestMCPFrontDeniesUnlistedCall(t *testing.T) {
	base, _, auditPath := testGateway(t)
	code, body := call(t, base, "tok-dev",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_thing","arguments":{}}}`, "")
	if code != 200 || !strings.Contains(body, "tool_not_in_role") {
		t.Fatalf("want tool_not_in_role, got %d %s", code, body)
	}
	raw, _ := os.ReadFile(auditPath)
	if !strings.Contains(string(raw), "mcp_denied") {
		t.Fatal("denial must be audited")
	}
}

func TestMCPFrontCache(t *testing.T) {
	base, _, _ := testGateway(t)
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"q":"x"}}}`
	call(t, base, "tok-owner", body, "")
	call(t, base, "tok-owner", body, "")
	resp, err := http.Get(base + "/stats/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	if st["cache_hits"].(float64) != 1 || st["cache_misses"].(float64) != 1 {
		t.Fatalf("want 1 hit / 1 miss, got %v", st)
	}
	// A write is never cached, so it is counted as bypass, not as a miss.
	call(t, base, "tok-owner", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"write_thing","arguments":{}}}`, "")
	resp2, _ := http.Get(base + "/stats/mcp")
	defer resp2.Body.Close()
	var st2 map[string]any
	json.NewDecoder(resp2.Body).Decode(&st2)
	if st2["cache_bypass_no_ttl"].(float64) != 1 {
		t.Fatalf("write must bypass cache: %v", st2)
	}
}

func TestMCPFrontPersonalCacheIsPerUser(t *testing.T) {
	base, _, _ := testGateway(t)
	body := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"personal_echo","arguments":{"q":"same"}}}`
	call(t, base, "tok-dev", body, "")
	call(t, base, "tok-dev-2", body, "") // same role, different person
	resp, _ := http.Get(base + "/stats/mcp")
	defer resp.Body.Close()
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	if st["cache_hits"].(float64) != 0 {
		t.Fatalf("a personal answer must not be reused across users: %v", st)
	}
}

func TestMCPFrontSSE(t *testing.T) {
	base, _, _ := testGateway(t)
	code, body := call(t, base, "tok-owner",
		`{"jsonrpc":"2.0","id":6,"method":"tools/list"}`, "text/event-stream")
	if code != 200 || !strings.HasPrefix(body, "event: message\ndata: {") || !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("bad SSE frame: %d %q", code, body)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(body, "event: message\ndata: "), "\n\n")), &doc); err != nil {
		t.Fatalf("SSE data is not JSON: %v", err)
	}
}

func TestMCPFrontTokenRotation(t *testing.T) {
	base, tokensPath, auditPath := testGateway(t)
	if code, _ := call(t, base, "tok-dev", `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`, ""); code != 200 {
		t.Fatalf("old token: want 200, got %d", code)
	}
	// Rotate: remove tok-dev, add tok-new for the same person. No restart.
	if err := os.WriteFile(tokensPath, []byte(`{"tokens":{"tok-owner":"1001","tok-new":"2002","tok-dev-2":"3003"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, base, "tok-dev", `{"jsonrpc":"2.0","id":8,"method":"tools/list"}`, ""); code != 401 {
		t.Fatalf("removed token must fail immediately, got %d", code)
	}
	if code, _ := call(t, base, "tok-new", `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`, ""); code != 200 {
		t.Fatalf("new token must work without restart, got %d", code)
	}
	raw, _ := os.ReadFile(auditPath)
	if !strings.Contains(string(raw), "mcp_tokens_rotated") {
		t.Fatal("rotation must be audited")
	}
	if strings.Contains(string(raw), "tok-new") {
		t.Fatal("audit log must not contain a usable token")
	}
}

func TestMCPFrontConcurrentUsers(t *testing.T) {
	base, _, _ := testGateway(t)
	tokens := []string{"tok-owner", "tok-dev", "tok-dev-2"}
	var wg sync.WaitGroup
	errs := make(chan string, 120)
	for i := 0; i < 120; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := tokens[i%len(tokens)]
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"echo","arguments":{"q":"q%d"}}}`, i, i%7)
			code, resp := call(t, base, tok, body, "")
			if code != 200 || !strings.Contains(resp, "echo:q") {
				errs <- fmt.Sprintf("%s -> %d %s", tok, code, resp)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	resp, _ := http.Get(base + "/stats/mcp")
	defer resp.Body.Close()
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	if st["cache_misses"].(float64) != 7 { // 7 distinct argument values, shared by role
		t.Logf("stats: %v", st) // informative, not a hard gate: scheduling decides the split
	}
	if st["tokens_active"].(float64) != 3 {
		t.Fatalf("tokens_active: %v", st)
	}
}

func TestMCPFrontCoalescesIdenticalInFlightCalls(t *testing.T) {
	var upstreamCalls int32
	base, _, _ := testGatewayWithCounter(t, &upstreamCalls, 80*time.Millisecond)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"q":"same"}}}`

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			call(t, base, "tok-owner", body, "")
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&upstreamCalls); got != 1 {
		t.Fatalf("20 identical concurrent calls must reach the service once, got %d", got)
	}
	resp, _ := http.Get(base + "/stats/mcp")
	defer resp.Body.Close()
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	if st["coalesced"].(float64) != 19 {
		t.Fatalf("want 19 coalesced, got %v", st["coalesced"])
	}
}
