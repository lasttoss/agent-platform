package main

// mcpfront serves the gateway to an MCP client (DeepSeek Harness and anything else that
// speaks Streamable HTTP). It is the same pipeline as /v1/respond with one difference that
// matters: the caller is an MCP client, so identity must arrive in the transport — a bearer
// token that maps to a user — and never as a tool argument the model could choose.
//
// Three things happen here, in this order: resolve identity, filter what the model may see,
// then (for a call) decide, cache, forward, record. Filtering before the call is what makes
// an unpermitted tool something the model never learns about.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type MCPFront struct {
	srv    *Server
	tokens *tokenSet
	jobs   *JobStore

	mu          sync.Mutex
	hits        int
	misses      int
	bypass      int // tool with ttl 0: never cached, so neither hit nor miss
	coalesced   int // identical calls in flight at the same moment, answered once
	sse         int
	inflight    map[string]*inflightCall
	denied      map[string]int
	quotaDenied int
	busyDenied  int
	rateLimited int
	byTool      map[string]int
}

// inflightCall is the single-flight slot for one cache key. Without it, twenty identical
// requests arriving together all miss the empty cache and all reach the upstream service —
// the cache only helps the *next* request, not the ones already in the air.
type inflightCall struct {
	done chan struct{}
	res  map[string]any
	err  error
}

func NewMCPFront(s *Server, tokens *tokenSet) *MCPFront {
	f := &MCPFront{srv: s, tokens: tokens, denied: map[string]int{}, byTool: map[string]int{},
		inflight: map[string]*inflightCall{}, jobs: NewJobStore(time.Hour)}
	go f.jobs.run(5 * time.Minute)
	return f
}

// PollJob answers GET /v1/jobs/{id}. It is authenticated by the same bearer token as /mcp, so a
// job is polled through the same door that created it — and only by the user who created it.
func (f *MCPFront) PollJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
		return
	}
	pal, ok := f.whoami(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	job, ok := f.jobs.get(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"), pal.UserID)
	if !ok {
		// Not 403: an id belonging to somebody else and an id that never existed must look the
		// same, or the endpoint becomes a way to test whether a job exists.
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such job"})
		return
	}
	writeJSON(w, http.StatusOK, job.view())
}

func (f *MCPFront) Stats() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := f.hits + f.misses
	rate := 0.0
	if total > 0 {
		rate = float64(f.hits) / float64(total)
	}
	return map[string]any{
		"cache_hits": f.hits, "cache_misses": f.misses, "cache_bypass_no_ttl": f.bypass,
		"cache_hit_rate": rate, "calls": f.byTool, "denied": f.denied,
		"tokens_active": f.tokens.Count(), "sse_responses": f.sse, "coalesced": f.coalesced,
		"quota_denied": f.quotaDenied, "busy_denied": f.busyDenied,
		"rate_limited": f.rateLimited, "jobs": f.jobs.stats(),
	}
}

func (f *MCPFront) whoami(r *http.Request) (Principal, bool) {
	h := r.Header.Get("Authorization")
	tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer"))
	if tok == "" || tok == h && !strings.HasPrefix(h, "Bearer") {
		tok = r.Header.Get("X-DSH-Token") // fallback for clients that cannot set Authorization
	}
	if tok == "" {
		return Principal{}, false
	}
	userID, ok, rep := f.tokens.Lookup(tok)
	if len(rep.Added) > 0 || len(rep.Removed) > 0 {
		f.srv.audit(map[string]any{"event": "mcp_tokens_rotated", "added": rep.Added,
			"removed": rep.Removed, "at": time.Now().UTC().Format(time.RFC3339)})
	}
	if !ok {
		return Principal{}, false
	}
	pal, err := f.srv.Policy.Resolve(userID, "dm")
	if err != nil {
		return Principal{}, false
	}
	// The console outranks the file: a role assigned there applies to the next request.
	if role, ok := f.srv.effectiveRole(userID); ok {
		pal.Role = role
	}
	return pal, true
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// emit answers in JSON, or as a one-event SSE stream when the client asks for one.
// Streamable HTTP permits either; a client that sends Accept: text/event-stream must not be
// handed JSON and left to guess.
func (f *MCPFront) emit(w http.ResponseWriter, r *http.Request, status int, payload any) {
	if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
		span.SetAttributes(attribute.Int("gateway.http_status", status))
		if errObj, ok := payload.(map[string]any)["error"].(map[string]any); ok {
			if code, ok := errObj["code"].(int); ok {
				span.SetAttributes(attribute.Int("gateway.rpc_error", code))
			}
		}
	}
	// A client that holds a session id must get it back on every reply; clients that ignore the
	// header are unaffected. The server keeps no session state either way.
	if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
		w.Header().Set("Mcp-Session-Id", sid)
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		raw, _ := json.Marshal(payload)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(status)
		w.Write([]byte("event: message\ndata: "))
		w.Write(raw)
		w.Write([]byte("\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		f.mu.Lock()
		f.sse++
		f.mu.Unlock()
		return
	}
	writeJSON(w, status, payload)
}

func rpcOK(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcErr(id json.RawMessage, code int, msg string, data any) map[string]any {
	e := map[string]any{"code": code, "message": msg}
	if data != nil {
		e["data"] = data
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": e}
}

func (f *MCPFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// One span for the request: metrics already say how many, this says which one was slow.
	// No user id goes into a span — a trace leaves this process by definition.
	ctx, span := StartServerSpan(r.Context(), r, "mcp")
	defer span.End()
	r = r.WithContext(ctx)
	if id := TraceID(ctx); id != "" {
		w.Header().Set("X-Trace-Id", id)
	}
	if r.Method != http.MethodPost {
		// Streamable HTTP allows a GET SSE stream; this gateway does not offer one, and saying
		// so is better than hanging a client that expects it.
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, rpcErr(nil, -32000, "POST only", nil))
		return
	}
	pal, ok := f.whoami(r)
	if !ok {
		f.srv.audit(map[string]any{"event": "mcp_auth_failed", "path": r.URL.Path,
			"at": time.Now().UTC().Format(time.RFC3339)})
		writeJSON(w, http.StatusUnauthorized, rpcErr(nil, -32001, "unauthorized", nil))
		return
	}

	var req mcpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcErr(nil, -32700, "parse error", nil))
		return
	}
	if req.ID == nil { // notification: no response body, but still no error
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		var initParams struct {
			ClientInfo map[string]any `json:"clientInfo"`
			Protocol   string         `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &initParams)
		f.srv.audit(map[string]any{"event": "mcp_initialize", "user": pal.UserID, "role": pal.Role,
			"client": initParams.ClientInfo, "protocol": initParams.Protocol,
			"at": time.Now().UTC().Format(time.RFC3339)})
		// Sessions are optional in MCP over HTTP, but some clients require the header. This
		// server keeps no session state; it answers with an id so those clients can proceed,
		// and echoes any id the client already holds.
		session := r.Header.Get("Mcp-Session-Id")
		if session == "" {
			var buf [16]byte
			if _, err := rand.Read(buf[:]); err == nil {
				session = hex.EncodeToString(buf[:])
			}
		}
		if session != "" {
			w.Header().Set("Mcp-Session-Id", session)
		}
		f.emit(w, r, http.StatusOK, rpcOK(req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"serverInfo":      map[string]any{"name": "agent-gateway", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"instructions":    "Tools are filtered by the caller's role. A tool you cannot see is a tool you may not call.",
		}))
	case "ping":
		f.emit(w, r, http.StatusOK, rpcOK(req.ID, map[string]any{}))
	case "tools/list":
		tools := []map[string]any{}
		for _, name := range f.srv.Policy.VisibleTools(pal, f.srv.Reg.Meta, f.srv.Reg, f.srv.Consent) {
			desc, schema, ok := f.srv.Reg.MCPToolDef(name)
			if !ok {
				continue
			}
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"name": name, "description": desc, "inputSchema": schema})
		}
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t["name"].(string))
		}
		// "Who asked to see what" is part of the audit, not a debug line: it is the only record
		// that a tool existed for a role at a moment in time.
		f.srv.audit(map[string]any{"event": "mcp_tools_list", "user": pal.UserID, "role": pal.Role,
			"count": len(names), "tools": names, "at": time.Now().UTC().Format(time.RFC3339)})
		f.emit(w, r, http.StatusOK, rpcOK(req.ID, map[string]any{"tools": tools}))
	case "tools/call":
		f.call(w, r, req, pal)
	default:
		f.emit(w, r, http.StatusOK, rpcErr(req.ID, -32601, "method not found: "+req.Method, nil))
	}
}

func (f *MCPFront) call(w http.ResponseWriter, r *http.Request, req mcpRequest, pal Principal) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		f.emit(w, r, http.StatusOK, rpcErr(req.ID, -32602, "invalid params", nil))
		return
	}
	// The tool is the unit of work: the cache decision, the upstream wait and the quota charge
	// all belong to this span, so a slow answer can be attributed without reading three logs.
	ctx, span := Tracer().Start(r.Context(), "mcp.tool",
		trace.WithAttributes(attribute.String("mcp.tool", params.Name),
			attribute.String("gateway.role", pal.Role), attribute.String("gateway.chat", pal.ChatType)))
	defer span.End()
	r = r.WithContext(ctx)

	meta, known := f.srv.Reg.Meta[params.Name]
	decision, err := f.srv.Policy.Decide(pal, params.Name, meta, known)
	if err != nil {
		f.bumpDenied(params.Name)
		f.srv.audit(map[string]any{"event": "mcp_denied", "user": pal.UserID, "role": pal.Role,
			"tool": params.Name, "reason": err.Error(), "at": time.Now().UTC().Format(time.RFC3339)})
		f.emit(w, r, http.StatusOK, rpcErr(req.ID, -32003, "denied: "+err.Error(), nil))
		return
	}

	// Arguments are checked here, before quota, cache or slot: a request that was never valid
	// should not spend the caller's allowance, and it must not reach the cache keyed by garbage.
	if _, inputSchema, ok := f.srv.Reg.MCPToolDef(params.Name); ok {
		if err := validateArgs(inputSchema, params.Arguments); err != nil {
			f.srv.audit(map[string]any{"event": "mcp_invalid_args", "user": pal.UserID,
				"role": pal.Role, "tool": params.Name, "reason": err.Error(),
				"at": time.Now().UTC().Format(time.RFC3339)})
			f.emit(w, r, http.StatusOK,
				rpcErr(req.ID, -32602, "invalid arguments: "+err.Error(), nil))
			return
		}
	}

	// The bucket comes before the daily cap: it is the part that keeps a burst from turning into
	// a cliff for everybody else on the same afternoon.
	if ok, retry := f.srv.limiter.allow(pal.UserID); !ok {
		f.mu.Lock()
		f.rateLimited++
		f.mu.Unlock()
		f.srv.audit(map[string]any{"event": "mcp_rate_limited", "user": pal.UserID,
			"role": pal.Role, "tool": params.Name, "retry_after_s": retry,
			"at": time.Now().UTC().Format(time.RFC3339)})
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		f.emit(w, r, http.StatusOK,
			rpcErr(req.ID, -32006, fmt.Sprintf("rate limited: retry in %ds", retry), nil))
		return
	}

	// The same daily quota as /v1/respond: an MCP client is another way to spend the allowance,
	// not a way around it. Charging happens after the answer, counting happens here.
	if f.srv.overQuota(pal.UserID) {
		f.mu.Lock()
		f.quotaDenied++
		f.mu.Unlock()
		f.srv.audit(map[string]any{"event": "mcp_quota_denied", "user": pal.UserID,
			"role": pal.Role, "tool": params.Name, "at": time.Now().UTC().Format(time.RFC3339)})
		f.emit(w, r, http.StatusOK, rpcErr(req.ID, -32004, "quota exceeded for today", nil))
		return
	}

	// A long tool becomes a job before the cache or the slot is touched: the request's whole job
	// is to hand back an id, and the in-flight slot must belong to the work, not to a caller
	// waiting for work that outlives the request.
	if meta.LongRunning {
		job, ok := f.jobs.create(pal.UserID, params.Name)
		if !ok {
			span.SetAttributes(attribute.String("gateway.outcome", "job_refused"))
			f.srv.audit(map[string]any{"event": "mcp_job_refused", "user": pal.UserID,
				"role": pal.Role, "tool": params.Name,
				"at": time.Now().UTC().Format(time.RFC3339)})
			f.emit(w, r, http.StatusOK, rpcErr(req.ID, -32007,
				fmt.Sprintf("too many running jobs (limit %d)", f.jobs.limit), nil))
			return
		}
		// The request is charged when it is accepted, the tokens when the answer exists: a job
		// that produced 4 MB of JSON did spend it.
		f.srv.chargeRequest(pal.UserID)
		span.SetAttributes(attribute.String("gateway.job", job.ID[:8]),
			attribute.String("gateway.outcome", "job"))
		f.srv.audit(map[string]any{"event": "mcp_job_started", "user": pal.UserID,
			"role": pal.Role, "tool": params.Name, "job": job.ID,
			"at": time.Now().UTC().Format(time.RFC3339)})

		go func() {
			started := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), toolTimeout(meta))
			defer cancel()
			release, ok := f.srv.acquire(params.Name, meta.MaxInFlight)
			if !ok {
				err := fmt.Errorf("busy: %s is saturated", params.Name)
				f.jobs.finish(job.ID, nil, err)
				f.srv.audit(map[string]any{"event": "mcp_job_failed", "user": pal.UserID,
					"role": pal.Role, "tool": params.Name, "job": job.ID, "error": err.Error(),
					"ms": time.Since(started).Milliseconds(),
					"at": time.Now().UTC().Format(time.RFC3339)})
				return
			}
			defer release()
			res, err := f.srv.Reg.CallToolCtx(ctx, params.Name, params.Arguments, pal.UserID)
			if err == nil {
				raw, _ := json.Marshal(res)
				f.srv.chargeTokens(pal.UserID, estimateTokens(string(raw)))
			}
			f.jobs.finish(job.ID, res, err)
			rec := map[string]any{"event": "mcp_job_done", "user": pal.UserID, "role": pal.Role,
				"tool": params.Name, "job": job.ID, "ms": time.Since(started).Milliseconds(),
				"at": time.Now().UTC().Format(time.RFC3339)}
			if err != nil {
				rec["event"] = "mcp_job_failed"
				rec["error"] = err.Error()
			}
			f.srv.audit(rec)
		}()

		body, _ := json.Marshal(map[string]any{"job_id": job.ID, "state": jobRunning,
			"tool": params.Name, "poll": "/v1/jobs/" + job.ID})
		f.emit(w, r, http.StatusOK, rpcOK(req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(body)}}}))
		return
	}

	// Personal tools are cached per user; a role-wide key would hand one person's answer to
	// the next person in the same role.
	scope := "role:" + pal.Role
	if meta.Personal {
		scope = "user:" + pal.UserID
	}
	key := hashKey(map[string]any{"tool": params.Name, "args": params.Arguments, "scope": scope,
		// A role assignment must be visible immediately; the generation in the key retires
		// every entry cached under the old permissions without walking the cache.
		"gen": f.srv.policyGeneration()})
	cacheable := meta.TTL > 0
	if cacheable {
		if e, hit := f.srv.Cache.GetExact(key); hit {
			span.SetAttributes(attribute.String("gateway.cache", "hit"))
			f.mu.Lock()
			f.hits++
			f.byTool[params.Name]++
			f.mu.Unlock()
			f.srv.chargeRequest(pal.UserID)
			f.srv.chargeTokens(pal.UserID, estimateTokens(e.Answer))
			f.srv.audit(map[string]any{"event": "mcp_call", "user": pal.UserID, "role": pal.Role,
				"tool": params.Name, "cache": "hit", "at": time.Now().UTC().Format(time.RFC3339)})
			f.emit(w, r, http.StatusOK, rpcOK(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": e.Answer}}}))
			return
		}
		// Not a miss yet: only the goroutine that becomes the leader counts one. A waiter is
		// counted as coalesced, so the three numbers add up to the calls that arrived.
	} else {
		span.SetAttributes(attribute.String("gateway.cache", "bypass"))
		f.mu.Lock()
		f.bypass++
		f.mu.Unlock()
	}

	// A saturated tool answers "busy" instead of queueing: queueing turns a slow service into a
	// slow caller, and a burst into a timeout.
	release, ok := f.srv.acquire(params.Name, meta.MaxInFlight)
	if !ok {
		f.mu.Lock()
		f.busyDenied++
		f.mu.Unlock()
		f.srv.audit(map[string]any{"event": "mcp_busy", "user": pal.UserID, "role": pal.Role,
			"tool": params.Name, "at": time.Now().UTC().Format(time.RFC3339)})
		f.emit(w, r, http.StatusOK, rpcErr(req.ID, -32005, "busy: "+params.Name+" is saturated", nil))
		return
	}
	defer release()

	started := time.Now()

	// One upstream call per identical in-flight request; the rest wait for that answer.
	var res map[string]any
	var callErr error
	if cacheable {
		f.mu.Lock()
		if slot, busy := f.inflight[key]; busy {
			f.mu.Unlock()
			<-slot.done
			span.SetAttributes(attribute.String("gateway.cache", "coalesced"))
			res, callErr = slot.res, slot.err
			f.mu.Lock()
			f.coalesced++
			f.byTool[params.Name]++
			f.mu.Unlock()
		} else {
			slot := &inflightCall{done: make(chan struct{})}
			f.inflight[key] = slot
			f.misses++
			f.mu.Unlock()
			span.SetAttributes(attribute.String("gateway.cache", "miss"),
				attribute.Bool("gateway.leader", true))
			res, callErr = f.srv.Reg.CallToolCtx(ctx, params.Name, params.Arguments, pal.UserID)
			slot.res, slot.err = res, callErr
			close(slot.done)
			f.mu.Lock()
			delete(f.inflight, key)
			f.byTool[params.Name]++
			f.mu.Unlock()
		}
	} else {
		res, callErr = f.srv.Reg.CallToolCtx(ctx, params.Name, params.Arguments, pal.UserID)
		f.mu.Lock()
		f.byTool[params.Name]++
		f.mu.Unlock()
	}

	span.SetAttributes(attribute.Int64("gateway.upstream_ms", time.Since(started).Milliseconds()))
	if callErr != nil {
		span.RecordError(callErr)
		span.SetStatus(codes.Error, "tool failed")
	}

	rec := map[string]any{"event": "mcp_call", "user": pal.UserID, "role": pal.Role,
		"tool": params.Name, "decision": decision, "ms": time.Since(started).Milliseconds(),
		"cache": map[bool]string{true: "miss", false: "bypass"}[cacheable],
		"at":    time.Now().UTC().Format(time.RFC3339)}
	if callErr != nil {
		rec["error"] = callErr.Error()
	}
	f.srv.audit(rec)
	if callErr != nil {
		f.emit(w, r, http.StatusOK, rpcErr(req.ID, -32000, callErr.Error(), nil))
		return
	}

	f.srv.chargeRequest(pal.UserID)
	raw, _ := json.Marshal(res)
	f.srv.chargeTokens(pal.UserID, estimateTokens(string(raw)))
	if cacheable {
		f.srv.Cache.PutExact(key, CacheEntry{Answer: string(raw), Saved: 1,
			Created: time.Now(), TTL: time.Duration(meta.TTL) * time.Second})
	}
	f.emit(w, r, http.StatusOK, rpcOK(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(raw)}}}))
}

// estimateTokens keeps the quota honest without a tokenizer: 4 bytes per token is the usual
// rule of thumb for JSON, and a quota that is approximately right beats one that is absent.
// It is an estimate and the docs say so.
func estimateTokens(s string) int {
	if n := len(s) / 4; n > 0 {
		return n
	}
	return 1
}

func (f *MCPFront) bumpDenied(tool string) {
	f.mu.Lock()
	f.denied[tool]++
	f.mu.Unlock()
}
