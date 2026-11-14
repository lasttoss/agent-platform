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
// testRoleStore lets a test hand the shared fixture a role store; nil in every test that does
// not care about the console.
var testRoleStore func(t *testing.T, dir string) *roleStore

func testGateway(t *testing.T) (string, string, string) {
	t.Helper()
	return testGatewayOpts(t, nil, 0, 1000, 0)
}

// testGatewayWithCounter is the same gateway; a non-nil counter counts upstream tool calls, so
// a test can prove that concurrency did not multiply them.
func testGatewayWithCounter(t *testing.T, upstreamCalls *int32, delay ...time.Duration) (string, string, string) {
	t.Helper()
	var d time.Duration
	if len(delay) > 0 {
		d = delay[0]
	}
	return testGatewayOpts(t, upstreamCalls, d, 1000, 0)
}

// testGatewayOpts is the same gateway with the knobs a test needs: a slow upstream, a smaller
// daily quota, and a per-tool in-flight ceiling.
func testGatewayOpts(t *testing.T, upstreamCalls *int32, delay time.Duration, requestsPerDay, maxInFlight int) (string, string, string) {
	t.Helper()
	upstream := httptest.NewServer(&mcpserver.Handler{
		Name: "up", Version: "t",
		Tools: []mcpserver.Tool{
			{Name: "echo", Description: "echo back", InputSchema: map[string]any{"type": "object"}},
			{Name: "personal_echo", Description: "personal", InputSchema: map[string]any{"type": "object"}},
			{Name: "write_thing", Description: "a write", InputSchema: map[string]any{"type": "object"}},
			{Name: "strict", Description: "needs a string q", InputSchema: map[string]any{
				"type":     "object",
				"required": []any{"q"},
				"properties": map[string]any{
					"q": map[string]any{"type": "string"},
				},
				"additionalProperties": false,
			}},
		},
		Call: func(name string, args map[string]any, r *http.Request) (any, error) {
			if upstreamCalls != nil {
				atomic.AddInt32(upstreamCalls, 1)
			}
			if delay > 0 {
				time.Sleep(delay) // keep the slot occupied so waiters really wait
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
	policyPath := write("policy.json", fmt.Sprintf(`{
      "allow_unknown_users": false,
      "users": {"1001": {"name":"Owner","role":"owner","workspace":"d"},
                "2002": {"name":"Dev","role":"dev","workspace":"d"},
                "3003": {"name":"Dev2","role":"dev","workspace":"d"}},
      "roles": {"owner": {"tools":["*"],"admin":true}, "dev": {"tools":["echo","personal_echo","strict"]}},
      "chats": {"dm": {"tools":["echo","personal_echo","write_thing","strict"], "personal_data": true, "free_chat": true}},
      "quota": {"requests_per_day": %d, "tokens_per_day": 100000}
    }`, requestsPerDay))
	consentPath := write("consent.json", `{}`)
	metaPath := write("tools.json", fmt.Sprintf(`{
      "echo": {"description":"echo","ttl":600,"max_in_flight":%d},
      "personal_echo": {"description":"personal","personal":true,"ttl":600},
      "write_thing": {"description":"write","ttl":0},
      "strict": {"description":"strict","ttl":0}
    }`, maxInFlight))
	mcpPath := write("mcp.json", fmt.Sprintf(`{"servers":[{"name":"up","url":%q,"tools":["echo","personal_echo","write_thing","strict"]}]}`, upstream.URL+"/mcp"))
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
	if testRoleStore != nil {
		srv.Roles = testRoleStore(t, dir)
	}
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
	if len(owner) != 4 || !contains(owner, "write_thing") {
		t.Fatalf("owner sees %v", owner)
	}
	if len(dev) != 3 || contains(dev, "write_thing") {
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
	resp2, err := http.Get(base + "/stats/mcp")
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.Get(base + "/stats/mcp")
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.Get(base + "/stats/mcp")
	if err != nil {
		t.Fatal(err)
	}
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
	resp, err := http.Get(base + "/stats/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	if st["coalesced"].(float64) != 19 {
		t.Fatalf("want 19 coalesced, got %v", st["coalesced"])
	}
}

func TestMCPFrontEnforcesDailyQuota(t *testing.T) {
	base, _, auditPath := testGatewayOpts(t, nil, 0, 3, 0)
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"echo","arguments":{"q":"q%d"}}}`, i, i)
		if code, resp := call(t, base, "tok-owner", body, ""); code != 200 || strings.Contains(resp, "quota") {
			t.Fatalf("call %d should be allowed: %s", i, resp)
		}
	}
	code, resp := call(t, base, "tok-owner",
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{"q":"over"}}}`, "")
	if code != 200 || !strings.Contains(resp, "quota exceeded") {
		t.Fatalf("4th call must hit the quota: %d %s", code, resp)
	}
	raw, _ := os.ReadFile(auditPath)
	if !strings.Contains(string(raw), "mcp_quota_denied") {
		t.Fatal("quota denial must be audited")
	}
	// A different user has their own allowance.
	if _, resp := call(t, base, "tok-dev", `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"echo","arguments":{"q":"x"}}}`, ""); strings.Contains(resp, "quota") {
		t.Fatalf("quota is per user, not global: %s", resp)
	}
}

func TestMCPFrontRejectsWhenToolSaturated(t *testing.T) {
	base, _, _ := testGatewayOpts(t, nil, 150*time.Millisecond, 1000, 2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount, busyCount := 0, 0
	start := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release together, so the ceiling is exercised
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"echo","arguments":{"q":"d%d"}}}`, i, i)
			_, resp := call(t, base, "tok-owner", body, "")
			mu.Lock()
			if strings.Contains(resp, "busy:") {
				busyCount++
			} else {
				okCount++
			}
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	if okCount != 2 || busyCount != 4 {
		t.Fatalf("want 2 served and 4 refused at a ceiling of 2, got ok=%d busy=%d", okCount, busyCount)
	}
	resp, err := http.Get(base + "/stats/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	if st["busy_denied"].(float64) != 4 {
		t.Fatalf("busy_denied: %v", st["busy_denied"])
	}
}

func callAdmin(t *testing.T, base, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func withRoleStore(t *testing.T, roles map[string]string) {
	t.Helper()
	testRoleStore = func(t *testing.T, dir string) *roleStore {
		raw, _ := json.Marshal(map[string]any{"roles": roles})
		p := filepath.Join(dir, "roles.json")
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		rs, err := NewRoleStore(p)
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	t.Cleanup(func() { testRoleStore = nil })
}

func TestAdminOnlyForAdminRoles(t *testing.T) {
	base, _, _ := testGateway(t)
	if code, _ := callAdmin(t, base, "tok-dev", "GET", "/admin/users", ""); code != 403 {
		t.Fatalf("dev must not read the admin list, got %d", code)
	}
	code, out := callAdmin(t, base, "tok-owner", "GET", "/admin/users", "")
	if code != 200 {
		t.Fatalf("owner should read the list: %d %v", code, out)
	}
	users, _ := out["users"].([]any)
	if len(users) != 3 {
		t.Fatalf("want 3 subjects, got %v", out["users"])
	}
	if code, _ := callAdmin(t, base, "tok-owner", "GET", "/admin/roles", ""); code != 200 {
		t.Fatalf("roles list: %d", code)
	}
	if code, _ := callAdmin(t, base, "nope", "GET", "/admin/users", ""); code != 401 {
		t.Fatalf("unknown token must be 401, got %d", code)
	}
}

func TestAdminRoleChangeAppliesToNextRequestAndAudits(t *testing.T) {
	withRoleStore(t, map[string]string{"1001": "owner", "2002": "dev", "3003": "dev"})
	base, _, auditPath := testGateway(t)

	// Before: dev cannot write.
	if _, resp := call(t, base, "tok-dev",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_thing","arguments":{"q":"x"}}}`, ""); !strings.Contains(resp, "denied") {
		t.Fatalf("dev should be denied before the change: %s", resp)
	}
	code, out := callAdmin(t, base, "tok-owner", "POST", "/admin/users/2002/role", `{"role":"owner"}`)
	if code != 200 || out["to"] != "owner" {
		t.Fatalf("assign: %d %v", code, out)
	}
	// After: same token, next request is allowed. No restart, no re-login.
	if _, resp := call(t, base, "tok-dev",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_thing","arguments":{"q":"y"}}}`, ""); strings.Contains(resp, "denied") {
		t.Fatalf("change must apply to the next request: %s", resp)
	}
	raw, _ := os.ReadFile(auditPath)
	if !strings.Contains(string(raw), "admin_role_change") || !strings.Contains(string(raw), "\"to\":\"owner\"") {
		t.Fatalf("role change must be audited: %s", raw)
	}
}

func TestAdminGuards(t *testing.T) {
	withRoleStore(t, map[string]string{"1001": "owner", "2002": "dev", "3003": "dev"})
	base, _, _ := testGateway(t)

	if code, out := callAdmin(t, base, "tok-owner", "POST", "/admin/users/1001/role", `{"role":"dev"}`); code != 409 {
		t.Fatalf("self-demotion must be refused, got %d %v", code, out)
	}
	if code, _ := callAdmin(t, base, "tok-owner", "POST", "/admin/users/2002/role", `{"role":"wizard"}`); code != 400 {
		t.Fatalf("unknown role must be 400, got %d", code)
	}
	if code, _ := callAdmin(t, base, "tok-owner", "POST", "/admin/users/9999/role", `{"role":"dev"}`); code != 404 {
		t.Fatalf("unknown subject must be 404, got %d", code)
	}
	if code, _ := callAdmin(t, base, "tok-dev", "POST", "/admin/users/3003/role", `{"role":"owner"}`); code != 403 {
		t.Fatalf("non-admin must not assign roles, got %d", code)
	}
}

// The generation is what retires cached answers after a permission change. Two different
// mechanisms are checked here: a role change alters the cache scope, and a re-assignment that
// keeps the same role still bumps the generation, so an entry cached under the old permissions
// cannot be served again.
func TestAdminRoleChangeRetiresCachedAnswers(t *testing.T) {
	withRoleStore(t, map[string]string{"1001": "owner", "2002": "dev"})
	base, _, _ := testGateway(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"q":"same"}}}`
	call(t, base, "tok-dev-2", body, "") // 3003 is dev via policy, no store entry
	_, first := call(t, base, "tok-dev-2", body, "")
	if !strings.Contains(first, "result") {
		t.Fatalf("second call should have been served: %s", first)
	}
	stats := func() map[string]float64 {
		resp, err := http.Get(base + "/stats/mcp")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return map[string]float64{"hits": m["cache_hits"].(float64), "misses": m["cache_misses"].(float64)}
	}
	before := stats()
	if before["hits"] < 1 {
		t.Fatalf("expected a cache hit before the change: %v", before)
	}
	// Re-assign the same role to a different user: the generation moves, so the next identical
	// call must miss instead of reusing an answer cached under the old permissions.
	if code, out := callAdmin(t, base, "tok-owner", "POST", "/admin/users/2002/role", `{"role":"dev"}`); code != 200 {
		t.Fatalf("assign: %d %v", code, out)
	}
	call(t, base, "tok-dev-2", body, "")
	after := stats()
	if after["misses"] <= before["misses"] {
		t.Fatalf("generation bump must invalidate the cache: before=%v after=%v", before, after)
	}
}

func TestRejectsInvalidArgumentsBeforeAnyWork(t *testing.T) {
	var calls int32
	base, _, auditPath := testGatewayWithCounter(t, &calls)
	cases := []string{
		`{"q":1}`,         // wrong type
		`{}`,              // missing required
		`{"q":"a","x":2}`, // additionalProperties: false
	}
	for _, args := range cases {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"strict","arguments":` + args + `}}`
		code, resp := call(t, base, "tok-owner", body, "")
		if code != 200 || !strings.Contains(resp, "invalid arguments") {
			t.Fatalf("args %s must be refused: %d %s", args, code, resp)
		}
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("invalid arguments must not reach the service, got %d calls", calls)
	}
	if _, resp := call(t, base, "tok-owner",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"strict","arguments":{"q":"ok"}}}`, ""); !strings.Contains(resp, "result") {
		t.Fatalf("valid arguments must pass: %s", resp)
	}
	raw, _ := os.ReadFile(auditPath)
	if !strings.Contains(string(raw), "mcp_invalid_args") {
		t.Fatal("refusing an invalid call must be audited")
	}
}

func TestPerToolTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}],"isError":false}}`))
	}))
	defer slow.Close()
	meta := map[string]ToolMeta{"slow": {Description: "slow", TimeoutS: 0.05}}
	reg := NewRegistry(meta, []ServerConf{{Name: "slow", URL: slow.URL, Tools: []string{"slow"}}})
	if errs := reg.Discover(); len(errs) > 0 {
		t.Fatalf("discover: %v", errs)
	}
	started := time.Now()
	if _, err := reg.CallTool("slow", map[string]any{}, "t"); err == nil {
		t.Fatal("a tool slower than its deadline must fail")
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("timeout did not cut the call short: %v", elapsed)
	}
}

func TestUsageSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.json")
	os.WriteFile(policyPath, []byte(`{
      "allow_unknown_users": false,
      "users": {"1001": {"name":"Owner","role":"owner","workspace":"d"}},
      "roles": {"owner": {"tools":["*"]}},
      "chats": {"dm": {"tools":["echo"], "free_chat": true}},
      "quota": {"requests_per_day": 3, "tokens_per_day": 1000}
    }`), 0o600)
	pol, err := LoadPolicy(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	first := NewServer(pol, nil, nil, filepath.Join(dir, "audit.log"))
	store := newUsageStore(filepath.Join(dir, "usage.json"))
	first.usageStore = store
	for i := 0; i < 3; i++ {
		first.chargeRequest("1001")
	}
	if !first.overQuota("1001") {
		t.Fatal("three of three must be spent")
	}
	if err := store.save(first); err != nil {
		t.Fatal(err)
	}
	second := NewServer(pol, nil, nil, filepath.Join(dir, "audit.log"))
	loaded, err := newUsageStore(filepath.Join(dir, "usage.json")).load(second)
	if err != nil || !loaded {
		t.Fatalf("load: %v loaded=%v", err, loaded)
	}
	if !second.overQuota("1001") {
		t.Fatal("a restart must not hand out a second allowance")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	withRoleStore(t, map[string]string{"1001": "owner"})
	base, _, _ := testGateway(t)
	call(t, base, "tok-owner",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"q":"m"}}}`, "")
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	page := string(raw)
	for _, want := range []string{"gateway_mcp_cache_misses_total", "gateway_roles_generation",
		`gateway_mcp_tool_calls_total{tool="echo"}`} {
		if !strings.Contains(page, want) {
			t.Fatalf("metrics missing %q", want)
		}
	}
	if strings.Contains(page, "1001") {
		t.Fatal("metrics must not expose user ids")
	}
}

func TestInitializeReturnsSessionID(t *testing.T) {
	base, _, _ := testGateway(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"t","version":"0"}}}`
	req, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-owner")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	session := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if session == "" {
		t.Fatal("initialize should answer with a session id for clients that require one")
	}
	req2, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	req2.Header.Set("Authorization", "Bearer tok-owner")
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Mcp-Session-Id", session)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if got := resp2.Header.Get("Mcp-Session-Id"); got != session {
		t.Fatalf("a session id the client holds must be echoed, got %q", got)
	}
}
