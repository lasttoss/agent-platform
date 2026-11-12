package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/lasttoss/agent-platform/services/gateway-go/internal/mcpserver"
)

// flakyService is one process with two faces: /health can be told to fail, while /mcp keeps
// working. That separation is the point of the test — it proves the gateway trusts the health
// answer, not a lucky tool list.
type flakyService struct {
	srv    *httptest.Server
	down   atomic.Bool
	listOK atomic.Int64
}

func newFlakyService(t *testing.T) *flakyService {
	t.Helper()
	f := &flakyService{}
	mcp := &mcpserver.Handler{Name: "flaky", Tools: []mcpserver.Tool{
		{Name: "docs_search", InputSchema: map[string]any{"type": "object"}},
	}, Call: func(string, map[string]any, *http.Request) (any, error) { return map[string]any{}, nil }}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	mux.Handle("/mcp", mcp)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// A service that was down at boot must appear when it comes up: the deploy order stops being
// something a human has to remember.
func TestRefreshFindsAServiceThatStartedLater(t *testing.T) {
	f := newFlakyService(t)
	f.down.Store(true)

	meta := map[string]ToolMeta{"docs_search": {}}
	reg := NewRegistry(meta, []ServerConf{{Name: "flaky", URL: f.srv.URL + "/mcp",
		Tools: []string{"docs_search"}}})

	rep := reg.Refresh()
	if reg.Advertises("docs_search") {
		t.Fatal("a service answering 503 on /health must not have its tools offered")
	}
	if len(rep.Errors) == 0 {
		t.Fatal("a failed probe must be reported, not swallowed")
	}

	f.down.Store(false)
	rep = reg.Refresh()
	if !reg.Advertises("docs_search") {
		t.Fatalf("tool should appear after the service recovers: %+v", rep)
	}
	if len(rep.Added) != 1 || rep.Added[0] != "docs_search" {
		t.Fatalf("the change should be reported once: %+v", rep.Added)
	}
	// A second refresh with nothing changed must report nothing: a log line per tick that says
	// "nothing happened" is how real changes stop being noticed.
	rep = reg.Refresh()
	if len(rep.Added) != 0 || len(rep.Removed) != 0 {
		t.Fatalf("no-change refresh reported noise: %+v", rep)
	}
}

// The reverse direction: a service that dies stops being an offer. This is what turns a slow
// timeout into a fast 503.
func TestRefreshDropsToolsWhenTheServiceDies(t *testing.T) {
	f := newFlakyService(t)
	meta := map[string]ToolMeta{"docs_search": {}}
	reg := NewRegistry(meta, []ServerConf{{Name: "flaky", URL: f.srv.URL + "/mcp",
		Tools: []string{"docs_search"}}})

	if rep := reg.Refresh(); !reg.Advertises("docs_search") {
		t.Fatalf("tool should be visible while healthy: %+v", rep)
	}
	f.down.Store(true)
	rep := reg.Refresh()
	if reg.Advertises("docs_search") {
		t.Fatal("tool must disappear when its service stops answering /health")
	}
	if len(rep.Removed) != 1 {
		t.Fatalf("removal should be reported: %+v", rep.Removed)
	}
}

// A service with no health endpoint at all is not an outage. Without this rule, every plain MCP
// server would have its tools deleted on the first tick.
func TestMissingHealthEndpointIsNotAnOutage(t *testing.T) {
	mcp := &mcpserver.Handler{Name: "plain", Tools: []mcpserver.Tool{
		{Name: "status", InputSchema: map[string]any{"type": "object"}},
	}, Call: func(string, map[string]any, *http.Request) (any, error) { return map[string]any{}, nil }}
	srv := httptest.NewServer(mcp) // GET /health answers the JSON-RPC error, not 200
	defer srv.Close()

	reg := NewRegistry(map[string]ToolMeta{"status": {}}, []ServerConf{
		{Name: "plain", URL: srv.URL + "/mcp", HealthURL: srv.URL + "/nonexistent", Tools: []string{"status"}}})
	reg.Refresh()
	if !reg.Advertises("status") {
		t.Fatal("a 404 on the health path means 'no health endpoint', not 'broken'")
	}
}
