// jira-mock is a stand-in for a real Jira MCP server: same wire shape, no network, no token.
// It exists so the gateway's RBAC and cache can be demonstrated without a Jira account.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lasttoss/agent-platform/services/gateway-go/internal/mcpserver"
)

type issue struct {
	Key     string `json:"key"`
	Summary string `json:"summary"`
	Status  string `json:"status"`
	Team    string `json:"team"`
	Created string `json:"created"`
	By      string `json:"created_by"`
}

var (
	mu     sync.Mutex
	nextID = 7
	issues = []issue{
		{"JIRA-101", "Checkout fails on iOS 17", "Open", "payments", "2026-11-01", "lan"},
		{"JIRA-102", "Latency spike on event ingest", "In Progress", "platform", "2026-11-03", "dung"},
		{"JIRA-103", "Add retry to webhook delivery", "Open", "platform", "2026-11-04", "dung"},
		{"JIRA-104", "Refund flow double-charges", "Done", "payments", "2026-10-28", "lan"},
		{"JIRA-105", "Dashboard shows stale CCU", "Open", "game", "2026-11-06", "minh"},
		{"JIRA-106", "Rotate signing keys", "Done", "security", "2026-10-20", "dung"},
	}
)

func text(v any) map[string]any {
	raw, _ := json.MarshalIndent(v, "", " ")
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(raw)}}}
}

func call(name string, args map[string]any, r *http.Request) (any, error) {
	caller := r.Header.Get("X-Caller-Id")
	time.Sleep(30 * time.Millisecond) // stand-in for a network hop
	mu.Lock()
	defer mu.Unlock()
	switch name {
	case "jira_search":
		q, _ := args["query"].(string)
		q = strings.ToLower(q)
		out := []issue{}
		for _, is := range issues {
			if q == "" || strings.Contains(strings.ToLower(is.Summary), q) ||
				strings.Contains(strings.ToLower(is.Team), q) || strings.Contains(strings.ToLower(is.Key), q) {
				out = append(out, is)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		return text(map[string]any{"issues": out, "count": len(out), "served_by": "jira-mock"}), nil
	case "jira_get_issue":
		key, _ := args["key"].(string)
		for _, is := range issues {
			if strings.EqualFold(is.Key, key) {
				return text(map[string]any{"issue": is}), nil
			}
		}
		return text(map[string]any{"error": "not found", "key": key}), nil
	case "jira_create_issue":
		summary, _ := args["summary"].(string)
		if strings.TrimSpace(summary) == "" {
			return text(map[string]any{"error": "summary is required"}), nil
		}
		team, _ := args["team"].(string)
		if team == "" {
			team = "platform"
		}
		is := issue{Key: fmt.Sprintf("JIRA-%d", nextID), Summary: summary, Status: "Open",
			Team: team, Created: time.Now().Format("2006-01-02"), By: caller}
		nextID++
		issues = append(issues, is)
		return text(map[string]any{"created": is, "requested_by": caller}), nil
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8092", "listen address")
	flag.Parse()
	h := &mcpserver.Handler{
		Name:    "jira-mock",
		Version: "0.1.0",
		Tools: []mcpserver.Tool{
			{Name: "jira_search", Description: "Search Jira issues by text, team or key.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{
					"query": map[string]any{"type": "string"}}, "required": []string{"query"}}},
			{Name: "jira_get_issue", Description: "Read one Jira issue by key.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{
					"key": map[string]any{"type": "string"}}, "required": []string{"key"}}},
			{Name: "jira_create_issue", Description: "Create a Jira issue.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{
					"summary": map[string]any{"type": "string"},
					"team":    map[string]any{"type": "string"}}, "required": []string{"summary"}}},
		},
		Call: call,
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	mux.Handle("/health", h)
	log.Printf("jira-mock listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
