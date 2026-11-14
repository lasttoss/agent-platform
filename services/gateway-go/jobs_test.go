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
	"sync/atomic"
	"testing"
	"time"

	"github.com/lasttoss/agent-platform/services/gateway-go/internal/mcpserver"
)

// jobGateway is the shared fixture with one long-running tool: the upstream sleeps, so the
// request cannot be the thing that waits for it.
func jobGateway(t *testing.T, sleep time.Duration, calls *int32) string {
	t.Helper()
	upstream := httptest.NewServer(&mcpserver.Handler{
		Name: "up", Version: "t",
		Tools: []mcpserver.Tool{
			{Name: "slow_build", Description: "takes minutes", InputSchema: map[string]any{"type": "object"}},
			{Name: "fast", Description: "quick", InputSchema: map[string]any{"type": "object"}},
		},
		Call: func(name string, args map[string]any, r *http.Request) (any, error) {
			if calls != nil {
				atomic.AddInt32(calls, 1)
			}
			if name == "slow_build" {
				time.Sleep(sleep)
			}
			return map[string]any{"content": []map[string]any{{"type": "text",
				"text": fmt.Sprintf(`{"tool":%q,"done":true}`, name)}}}, nil
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
                "2002": {"name":"Dev","role":"dev","workspace":"d"}},
      "roles": {"owner": {"tools":["*"],"admin":true}, "dev": {"tools":["fast"]}},
      "chats": {"dm": {"tools":["fast","slow_build"], "personal_data": true, "free_chat": true}},
      "quota": {"requests_per_day": 1000, "tokens_per_day": 100000}
    }`)
	metaPath := write("tools.json", `{
      "slow_build": {"description":"slow","ttl":0,"timeout_s":30,"long_running":true},
      "fast": {"description":"fast","ttl":600}
    }`)
	mcpPath := write("mcp.json", fmt.Sprintf(`{"servers":[{"name":"up","url":%q,"tools":["slow_build","fast"]}]}`,
		upstream.URL+"/mcp"))
	tokensPath := write("tokens.json", `{"tokens":{"tok-owner":"1001","tok-dev":"2002"}}`)

	pol, err := LoadPolicy(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	consent, err := LoadConsent(write("consent.json", `{}`))
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
	base := httptest.NewServer(srv.Routes())
	t.Cleanup(base.Close)
	return base.URL
}

func pollJob(t *testing.T, base, token, id string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/jobs/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	json.Unmarshal(raw, &body)
	return resp.StatusCode, body
}

// The point of a job: the call comes back in milliseconds while the work continues, and the
// answer is still there afterwards.
func TestLongToolReturnsJobIDAndFinishes(t *testing.T) {
	base := jobGateway(t, 400*time.Millisecond, nil)

	start := time.Now()
	code, body := call(t, base, "tok-owner",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow_build","arguments":{}}}`, "")
	took := time.Since(start)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if took > 300*time.Millisecond {
		t.Fatalf("the request waited %v for work that takes 400ms", took)
	}

	var reply struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		t.Fatalf("reply: %v (%s)", err, body)
	}
	var job struct {
		ID    string `json:"job_id"`
		State string `json:"state"`
		Poll  string `json:"poll"`
	}
	if err := json.Unmarshal([]byte(reply.Result.Content[0].Text), &job); err != nil {
		t.Fatalf("job envelope: %v (%s)", err, reply.Result.Content[0].Text)
	}
	if job.ID == "" || job.State != jobRunning || job.Poll != "/v1/jobs/"+job.ID {
		t.Fatalf("unexpected envelope %+v", job)
	}

	// Polling while it runs must say "running", not lie about an empty answer.
	if code, first := pollJob(t, base, "tok-owner", job.ID); code != 200 || first["state"] != jobRunning {
		t.Fatalf("first poll: %d %v", code, first)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		code, st := pollJob(t, base, "tok-owner", job.ID)
		if code != 200 {
			t.Fatalf("poll: %d %v", code, st)
		}
		if st["state"] == jobDone {
			if st["result"] == nil {
				t.Fatalf("done without a result: %v", st)
			}
			if ms, _ := st["ms"].(float64); ms < 300 {
				t.Fatalf("a 400ms tool reported %v ms", ms)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never finished: %v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A job id is a capability: the wrong user gets the same answer as a made-up id.
func TestJobIsInvisibleToAnotherUser(t *testing.T) {
	base := jobGateway(t, 50*time.Millisecond, nil)
	_, body := call(t, base, "tok-owner",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow_build","arguments":{}}}`, "")
	id := extractJobID(t, body)

	if code, _ := pollJob(t, base, "tok-dev", id); code != 404 {
		t.Fatalf("another user got %d, want 404", code)
	}
	if code, _ := pollJob(t, base, "tok-owner", "deadbeef"); code != 404 {
		t.Fatalf("unknown id got %d, want 404", code)
	}
	// No token at all is 401, not 404: the door is the first check.
	if req, _ := http.NewRequest(http.MethodGet, base+"/v1/jobs/"+id, nil); req != nil {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("no token got %d, want 401", resp.StatusCode)
		}
	}
}

// A burst of jobs is a burst of goroutines, so the store refuses past the per-user limit instead
// of queueing work nobody will read.
func TestJobLimitRefusesInsteadOfQueueing(t *testing.T) {
	base := jobGateway(t, 300*time.Millisecond, nil)
	refused := 0
	for i := 0; i < 6; i++ {
		_, body := call(t, base, "tok-owner",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow_build","arguments":{}}}`, "")
		if strings.Contains(body, "-32007") || strings.Contains(body, "too many running jobs") {
			refused++
		}
	}
	if refused == 0 {
		t.Fatalf("six jobs against a limit of four were all accepted")
	}
}

func TestJobStoreDropsFinishedJobsAfterTTL(t *testing.T) {
	s := NewJobStore(time.Millisecond)
	job, ok := s.create("1001", "slow_build")
	if !ok {
		t.Fatal("create refused")
	}
	s.finish(job.ID, map[string]any{"ok": true}, nil)
	if _, ok := s.get(job.ID, "1001"); !ok {
		t.Fatal("job vanished before the sweep")
	}
	time.Sleep(5 * time.Millisecond)
	s.sweep(time.Now())
	if _, ok := s.get(job.ID, "1001"); ok {
		t.Fatal("finished job survived the sweep")
	}
	// The per-user running count must be released, or one user is locked out after four jobs.
	if st := s.stats(); st["running"].(int) != 0 {
		t.Fatalf("running count leaked: %v", st)
	}
}

func extractJobID(t *testing.T, body string) string {
	t.Helper()
	var reply struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		t.Fatalf("reply: %v (%s)", err, body)
	}
	var job struct {
		ID string `json:"job_id"`
	}
	json.Unmarshal([]byte(reply.Result.Content[0].Text), &job)
	return job.ID
}
