package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Prometheus text exposition, hand-rolled: one endpoint, no dependency, and the numbers are
// already counted for /stats. What is exposed is deliberately operational, not personal: tool
// names and roles, never user ids.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	line("# HELP gateway_uptime_seconds Seconds since the process started.")
	line("# TYPE gateway_uptime_seconds gauge")
	line("gateway_uptime_seconds %.0f", uptimeSeconds())

	if s.MCP != nil {
		st := s.MCP.Stats()
		type metric struct{ key, name, help, typ string }
		for _, m := range []metric{
			{"cache_hits", "gateway_mcp_cache_hits_total", "Tool results served from cache.", "counter"},
			{"cache_misses", "gateway_mcp_cache_misses_total", "Tool results fetched from a service.", "counter"},
			{"coalesced", "gateway_mcp_calls_coalesced_total", "Requests joined to an in-flight identical call.", "counter"},
			{"sse_responses", "gateway_mcp_sse_responses_total", "Replies framed as a single SSE event.", "counter"},
			{"quota_denied", "gateway_mcp_quota_denied_total", "Calls refused by the daily quota.", "counter"},
			{"rate_limited", "gateway_mcp_rate_limited_total", "Calls refused by the per-user bucket.", "counter"},
			{"busy_denied", "gateway_mcp_busy_denied_total", "Calls refused because the tool was saturated.", "counter"},
			{"cache_bypass_no_ttl", "gateway_mcp_cache_bypass_total", "Calls to tools that are never cached.", "counter"},
			{"tokens_active", "gateway_mcp_tokens_active", "Bearer tokens the file currently accepts.", "gauge"},
		} {
			v, ok := st[m.key]
			if !ok {
				continue
			}
			line("# HELP %s %s", m.name, m.help)
			line("# TYPE %s %s", m.name, m.typ)
			line("%s %v", m.name, v)
		}

		if jobs, ok := st["jobs"].(map[string]any); ok {
			for _, m := range []struct{ key, name, help, typ string }{
				{"running", "gateway_jobs_running", "Jobs in flight in this process.", "gauge"},
				{"started", "gateway_jobs_started_total", "Jobs accepted.", "counter"},
				{"done", "gateway_jobs_done_total", "Jobs that produced an answer.", "counter"},
				{"failed", "gateway_jobs_failed_total", "Jobs that ended in an error.", "counter"},
				{"refused", "gateway_jobs_refused_total", "Jobs refused because the per-user limit was reached.", "counter"},
			} {
				line("# HELP %s %s", m.name, m.help)
				line("# TYPE %s %s", m.name, m.typ)
				line("%s %v", m.name, jobs[m.key])
			}
		}

		if calls, ok := st["calls"].(map[string]int); ok {
			tools := make([]string, 0, len(calls))
			for t := range calls {
				tools = append(tools, t)
			}
			sort.Strings(tools)
			line("# HELP gateway_mcp_tool_calls_total Calls per tool.")
			line("# TYPE gateway_mcp_tool_calls_total counter")
			for _, t := range tools {
				line("gateway_mcp_tool_calls_total{tool=%q} %d", t, calls[t])
			}
		}
		if denied, ok := st["denied"].(map[string]int); ok {
			tools := make([]string, 0, len(denied))
			for t := range denied {
				tools = append(tools, t)
			}
			sort.Strings(tools)
			line("# HELP gateway_mcp_denied_total Calls refused by policy, per tool.")
			line("# TYPE gateway_mcp_denied_total counter")
			for _, t := range tools {
				line("gateway_mcp_denied_total{tool=%q} %d", t, denied[t])
			}
		}
	}

	if s.Roles != nil {
		line("# HELP gateway_roles_generation Policy generation: every role change increments it.")
		line("# TYPE gateway_roles_generation gauge")
		line("gateway_roles_generation %d", s.Roles.Generation())
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(b.String()))
}
