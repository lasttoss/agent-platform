package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const PolicyVersion = "v1"

// Keyword routing is the no-model path: it needs no provider, no key and no tokens.
// Matching is on folded text, so the keys here are written without diacritics.
type intent struct {
	keys []string
	tool string
}

var intents = []intent{
	{[]string{"luong", "salary", "payroll"}, "salary_self"},
	{[]string{"tai lieu", "docs", "runbook", "search"}, "docs_search"},
	{[]string{"trang thai", "status", "health"}, "status"},
	// Reading a job back is a second turn by design: extraction is slow, a chat reply is not.
	{[]string{"ket qua", "result", "xong chua", "job"}, "ocr_result"},
	// A tool served by a TypeScript process: the intent table is the only place the gateway
	// mentions it, and that is the point.
	{[]string{"wiki", "tai lieu du an", "adr"}, "wiki_search"},
}

var pathRE = regexp.MustCompile(`[\w./-]+\.(?:pdf|png|jpe?g|tiff?|bmp|webp)`)

type Server struct {
	Policy  *Policy
	Consent map[string]map[string][]string
	Reg     *Registry
	Cache   *Cache
	Audit   string

	// AdapterToken is what makes "identity comes from the transport" true on HTTP too. Without
	// it, anyone who can reach /v1/respond can claim to be any user in the policy — loopback
	// binding is a network answer to an authorization question, and it stops being enough the
	// moment the gateway is not on the same host as its adapter.
	AdapterToken string

	// MCP is the same pipeline exposed to an MCP client (DeepSeek Harness, Claude Desktop).
	// Identity arrives as a bearer token in the transport, never as a tool argument.
	MCP *MCPFront

	// limiter is the per-user token bucket in front of the daily cap. Nil when the policy sets
	// requests_per_minute to 0.
	limiter *rateLimiter

	// usageStore persists today's counters so a restart does not hand out a second allowance.
	usageStore usageBackend

	// Roles is the console's answer to "who is a dev today", layered over policy.json. Nil
	// means the policy file alone decides, which is the behaviour of every earlier version.
	Roles *roleStore

	mu    sync.Mutex
	usage map[string]map[string]int // userID → {"requests", "tokens"}
	sems  map[string]chan struct{}  // tool → in-flight slots (see acquire)
	// jobs remembers the last extraction job per (user, chat), so the follow-up question
	// "result?" does not require the caller to copy an opaque id out of a message.
	jobs map[string]string

	// The user directory is the authority on *who exists*, and it is refreshed on the same
	// timer as service discovery. `dirSource` says where the current answer came from, because
	// "the directory is unreachable, so policy.json decides" must be a visible state, not a
	// quiet widening of access.
	dir       map[string]bool
	dirLoaded bool
	dirSource string
	dirStale  bool

	// The local JSONL file is the durable audit record. The sink is the aggregate view: best
	// effort, never blocking a request, and losing a delivery must not lose the line.
	auditQ       chan []byte
	auditDropped atomic.Int64
}

var startedAt = time.Now()

func uptimeSeconds() float64 { return time.Since(startedAt).Seconds() }

func NewServer(p *Policy, consent map[string]map[string][]string, reg *Registry, auditPath string) *Server {
	return &Server{Policy: p, Consent: consent, Reg: reg, Cache: NewCache(), Audit: auditPath,
		limiter: newRateLimiter(p.Quota.RequestsPerMinute, p.Quota.Burst),
		usage:   map[string]map[string]int{}, sems: map[string]chan struct{}{},
		jobs: map[string]string{}, dir: map[string]bool{}, dirSource: "policy"}
}

type respondRequest struct {
	PlatformUserID string `json:"platform_user_id"`
	ChatType       string `json:"chat_type"`
	Text           string `json:"text"`
}

// StartAuditSink ships each audit line to a collector, asynchronously. A full buffer drops the
// delivery and counts it: the file still has the line, and a request must never wait on logging.
func newEventID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) StartAuditSink(url string) {
	if url == "" {
		return
	}
	s.auditQ = make(chan []byte, 1024)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		for line := range s.auditQ {
			resp, err := client.Post(url, "application/json", bytes.NewReader(line))
			if err != nil {
				s.auditDropped.Add(1)
				continue
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				s.auditDropped.Add(1)
			}
		}
	}()
}

// AuditDropped is the number of sink deliveries that failed or were shed. Exposed so a deployment
// can alert on it instead of assuming every line arrived.
func (s *Server) AuditDropped() int64 { return s.auditDropped.Load() }

// SetUserActive writes to the directory through the same MCP tool an operator would call. This is
// deliberately not reachable from a chat: `user_set_active` is marked destructive in tools.json,
// and the gateway calls it here on behalf of a flag, with an audit line that says who did it.
func (s *Server) SetUserActive(reg *Registry, userID string, active bool) (map[string]any, error) {
	out, err := reg.CallTool("user_set_active",
		map[string]any{"user_id": userID, "active": active}, "gateway")
	if err != nil {
		return nil, err
	}
	s.audit(map[string]any{"user": userID, "decision": "directory_write",
		"active": active, "by": "operator"})
	return out, nil
}

func (s *Server) audit(rec map[string]any) {
	rec["ts"] = time.Now().Format(time.RFC3339)
	// An event id, not a content hash: two requests in the same second can look identical, and a
	// collector that dedupes on content would silently undercount them. A retried delivery keeps
	// the id, which is exactly what makes it a retry.
	rec["event_id"] = newEventID()
	raw, _ := json.Marshal(rec)
	if s.Audit != "" {
		if f, err := os.OpenFile(s.Audit, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Write(append(raw, '\n'))
			f.Close()
		}
	}
	if s.auditQ == nil {
		return
	}
	// Never block a request on logging: shed the delivery, keep the line, count the loss.
	select {
	case s.auditQ <- raw:
	default:
		s.auditDropped.Add(1)
	}
}

// One request slot per caller turn, tokens as they are actually spent. The two are counted
// separately: charging a request inside the model branch halved the daily quota whenever a
// model service was attached — a bug parity found at 300 generated cases, not at 10.
func (s *Server) chargeRequest(userID string) {
	s.mu.Lock()
	if s.usage[userID] == nil {
		s.usage[userID] = map[string]int{}
	}
	s.usage[userID]["requests"]++
	store := s.usageStore
	s.mu.Unlock()
	if store != nil {
		store.mark()
	}
}

func (s *Server) chargeTokens(userID string, tokens int) {
	s.mu.Lock()
	if s.usage[userID] == nil {
		s.usage[userID] = map[string]int{}
	}
	s.usage[userID]["tokens"] += tokens
	store := s.usageStore
	s.mu.Unlock()
	if store != nil {
		store.mark()
	}
}

func (s *Server) overQuota(userID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.usage[userID]
	return u["requests"] >= s.Policy.Quota.RequestsPerDay || u["tokens"] >= s.Policy.Quota.TokensPerDay
}

// acquire takes an in-flight slot or reports that the tool is saturated. It never waits: a
// gateway that queues turns a slow service into a slow caller, and turns a burst into a timeout.
func (s *Server) acquire(tool string, limit int) (func(), bool) {
	if limit <= 0 {
		return func() {}, true
	}
	s.mu.Lock()
	sem, ok := s.sems[tool]
	if !ok {
		sem = make(chan struct{}, limit)
		s.sems[tool] = sem
	}
	s.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
		return nil, false
	}
}

// LoadDirectory replaces the in-memory copy from the userdir service. Called at boot and by the
// refresh loop. A failure keeps the previous copy: an unreachable directory must not silently
// re-enable everybody.
func (s *Server) LoadDirectory(users map[string]bool) {
	s.mu.Lock()
	s.dir = users
	s.dirLoaded = true
	s.dirSource = "userdir"
	s.dirStale = false
	s.mu.Unlock()
}

// RefreshDirectory reads the directory over MCP. A failure keeps the last copy and marks it
// stale rather than reverting to policy: an unreachable Postgres must not re-enable a person
// somebody disabled on purpose.
func (s *Server) RefreshDirectory(reg *Registry) error {
	out, err := reg.CallTool("user_list", map[string]any{}, "gateway")
	if err != nil {
		s.mu.Lock()
		if s.dirLoaded {
			s.dirStale = true
		}
		s.mu.Unlock()
		return err
	}
	users := map[string]bool{}
	if list, ok := out["users"].([]any); ok {
		for _, item := range list {
			row, _ := item.(map[string]any)
			active, _ := row["active"].(bool)
			users[asString(row["id"])] = active
		}
	}
	s.LoadDirectory(users)
	return nil
}

// CheckUsers compares the directory against the users this gateway's policy provisions. It is
// the command a deployment runs after onboarding, and the one that catches an offboarding that
// only happened in one of the two systems.
func (s *Server) CheckUsers(reg *Registry) (map[string]any, error) {
	ids := make([]any, 0, len(s.Policy.Users))
	for id := range s.Policy.Users {
		ids = append(ids, id)
	}
	sort.Strings(make([]string, 0)) // keep the import honest; ids are sorted below
	sort.Slice(ids, func(i, j int) bool { return asString(ids[i]) < asString(ids[j]) })
	return reg.CallTool("user_consistency", map[string]any{"known_ids": ids}, "gateway")
}

// directoryVerdict answers "may this person act at all", which is a different question from
// "which tools may they use". A person disabled in Postgres is refused here, before any tool,
// any model call and any cache lookup.
func (s *Server) directoryVerdict(userID string) (allowed bool, code, detail string) {
	s.mu.Lock()
	dir, loaded := s.dir, s.dirLoaded
	s.mu.Unlock()
	if !loaded {
		return true, "", ""
	}
	active, known := dir[userID]
	if !known {
		return false, "user_not_in_directory", "the user directory does not know this caller"
	}
	if !active {
		return false, "user_disabled", "this account is disabled in the user directory"
	}
	return true, "", ""
}

func jobKey(pal Principal) string { return pal.UserID + "|" + pal.ChatType }

// rememberJob stores the id the service handed back. The mapping is per (user, chat): one
// person's pending extraction is not another person's, and a group is not a DM.
func (s *Server) rememberJob(pal Principal, id string) {
	s.mu.Lock()
	s.jobs[jobKey(pal)] = id
	s.mu.Unlock()
}

func (s *Server) pendingJob(pal Principal) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[jobKey(pal)]
}

func pickTool(text string) (string, map[string]any, bool) {
	low := Fold(text)
	if pathRE.MatchString(text) {
		return "ocr_extract", map[string]any{"path": pathRE.FindString(text)}, true
	}
	for _, in := range intents {
		for _, k := range in.keys {
			if strings.Contains(low, k) {
				return in.tool, map[string]any{"q": "runbook"}, true
			}
		}
	}
	return "", nil, false
}

func (s *Server) ttl(tool string) time.Duration {
	if m, ok := s.Reg.Meta[tool]; ok {
		return time.Duration(m.TTL) * time.Second
	}
	return 0
}

func scopeFor(pal Principal, tool string, meta map[string]ToolMeta) string {
	personal := pal.PersonalOK
	if m, ok := meta[tool]; ok && m.Personal {
		personal = true
	}
	if personal {
		return "user:" + pal.UserID
	}
	return "workspace:default"
}

// Handle is the whole decision path: identity → quota → which tools exist → cache → tool → delivery.
func (s *Server) Handle(req respondRequest) (int, map[string]any) {
	// Same bucket as /mcp: a limit that only covers one door is a suggestion.
	if ok, retry := s.limiter.allow(req.PlatformUserID); !ok {
		s.audit(map[string]any{"event": "rate_limited", "user": req.PlatformUserID,
			"retry_after_s": retry, "at": time.Now().UTC().Format(time.RFC3339)})
		return http.StatusTooManyRequests, map[string]any{
			"denied": map[string]any{"code": "rate_limited",
				"reason": fmt.Sprintf("retry in %ds", retry)}, "retry_after_s": retry}
	}
	if allowed, code, detail := s.directoryVerdict(req.PlatformUserID); !allowed {
		s.audit(map[string]any{"user": req.PlatformUserID, "chat": req.ChatType,
			"decision": "denied", "code": code})
		return 403, map[string]any{"denied": map[string]any{"code": code, "reason": detail}}
	}
	pal, err := s.Policy.Resolve(req.PlatformUserID, req.ChatType)
	if err != nil {
		d := err.(Denied)
		s.audit(map[string]any{"user": req.PlatformUserID, "chat": req.ChatType,
			"decision": "denied", "code": d.Code})
		return 403, map[string]any{"denied": map[string]any{"code": d.Code, "reason": d.Message}}
	}
	if s.overQuota(pal.UserID) {
		s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType, "decision": "quota_exceeded"})
		return 429, map[string]any{"denied": map[string]any{"code": "quota_exceeded",
			"reason": "daily quota reached"}}
	}

	s.chargeRequest(pal.UserID)
	visible := s.Policy.VisibleTools(pal, s.Reg.Meta, s.Reg, s.Consent)
	routable := s.Policy.RoutableTools(pal, s.Reg.Meta, s.Reg, s.Consent)

	tool, args, matched := pickTool(req.Text)
	ttl := time.Duration(0)
	if matched {
		ttl = s.ttl(tool)
	}
	scope := scopeFor(pal, tool, s.Reg.Meta)
	if matched && !contains(routable, tool) {
		// Configured in policy, but no running service advertises it. Answering "I have no tool
		// for this" would be a lie of the wrong kind: the capability exists, it is down.
		if !s.Reg.Advertises(tool) && s.Policy.ConfiguredFor(pal, tool, s.Reg.Meta, s.Consent) {
			s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
				"decision": "tool_unavailable", "tool": tool})
			return 503, map[string]any{"denied": map[string]any{
				"code": "service_unavailable", "reason": "no service advertises " + tool}}
		}
		matched = false
	}
	key := ExactKey(req.Text, visible, PolicyVersion, pal.ChatType, scope)
	if ttl > 0 {
		if hit, ok := s.Cache.GetExact(key); ok {
			s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
				"decision": "cache_exact", "scope": scope, "tool": tool})
			return 200, map[string]any{"delivered": "chat", "chat_reply": hit.Answer,
				"dm": nil, "tools_visible": visible,
				"cache": map[string]any{"layer": "exact", "saved_tokens": hit.Saved}}
		}
		if hit, sim, ok := s.Cache.GetSemantic(req.Text, scope, tool); ok {
			s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
				"decision": "cache_semantic", "scope": scope, "tool": tool})
			return 200, map[string]any{"delivered": "chat", "chat_reply": hit.Answer,
				"dm": nil, "tools_visible": visible,
				"cache": map[string]any{"layer": "semantic", "saved_tokens": hit.Saved,
					"similarity": sim}}
		}
	}

	// With a model service attached, the model picks the tool — from the visible list only.
	if s.Reg.Model != nil {
		schemas := []map[string]any{}
		for _, t := range visible {
			if sch := s.Reg.Schema(t); sch != nil {
				schemas = append(schemas, sch)
			}
		}
		turn, err := s.Reg.Complete(systemPrompt, req.Text, schemas)
		if err == nil {
			s.chargeTokens(pal.UserID, turn.PromptTokens+turn.CompletionTokens)
			if len(turn.ToolCalls) > 0 {
				blocked := ""
				for _, c := range turn.ToolCalls {
					if !contains(routable, c.Name) {
						blocked = c.Name
						break
					}
				}
				if blocked != "" {
					s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
						"decision": "tool_not_allowed", "tool": blocked, "offered": visible})
					return 200, map[string]any{"delivered": "chat", "dm": nil,
						"chat_reply":  "I don't have permission to do that here.",
						"denied_tool": blocked, "tools_visible": visible,
						"tools_offered": len(schemas)}
				}
				tool, args, matched = turn.ToolCalls[0].Name, turn.ToolCalls[0].Args, true
			} else if turn.Content != "" && s.Policy.FreeChat(pal) {
				s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
					"decision": "free_chat", "tools_visible": visible})
				return 200, map[string]any{"delivered": "chat", "chat_reply": turn.Content,
					"dm": nil, "tools_visible": visible, "tools_offered": len(schemas)}
			}
		}
	}

	if !matched {
		s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
			"decision": "no_tool", "tools_visible": visible})
		return 200, map[string]any{"delivered": "chat",
			"chat_reply": "I have no tool for that.", "dm": nil,
			"tools_visible": visible}
	}

	// A tool whose argument is not something a person should have to type: the gateway owns the
	// state, so it fills the argument. The model never chooses a job id, same rule as identity.
	if tool == "ocr_result" {
		id := s.pendingJob(pal)
		if id == "" {
			s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
				"decision": "no_tool", "tool": tool, "reason": "no_pending_job"})
			return 200, map[string]any{"delivered": "chat", "dm": nil,
				"chat_reply":    "No extraction job is pending. Send me a document first.",
				"tools_visible": visible}
		}
		args = map[string]any{"job_id": id}
	}

	meta, known := s.Reg.Meta[tool]
	mode, err := s.Policy.Decide(pal, tool, meta, known)
	if err != nil {
		d := err.(Denied)
		s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
			"decision": "denied", "code": d.Code, "tool": tool})
		return 403, map[string]any{"denied": map[string]any{"code": d.Code, "reason": d.Message}}
	}

	release, admitted := s.acquire(tool, meta.MaxInFlight)
	if !admitted {
		s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
			"decision": "shed", "tool": tool, "reason": "in_flight_limit"})
		return 503, map[string]any{"denied": map[string]any{
			"code": "service_busy", "reason": "too many calls in flight for " + tool}}
	}
	defer release()

	// The gateway executes nothing itself: one MCP call, same shape for every capability.
	result, callErr := s.Reg.CallTool(tool, args, pal.UserID)
	if callErr == nil && tool == "ocr_extract" {
		if id := asString(result["job_id"]); id != "" {
			s.rememberJob(pal, id)
		}
	}
	answer := render(tool, result, callErr)
	if ttl > 0 && callErr == nil {
		s.Cache.PutExact(key, CacheEntry{Answer: answer, TTL: ttl})
		s.Cache.PutSemantic(req.Text, answer, 0, ttl, scope, tool)
	}

	if mode == "private" {
		s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
			"decision": "private_delivery", "tool": tool})
		return 200, map[string]any{"delivered": "dm",
			"chat_reply":    "Sending that to you privately.",
			"dm":            map[string]any{"text": answer, "reason": "personal_data_in_shared_chat"},
			"tools_visible": visible}
	}
	s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
		"decision": "answered", "tool": tool})
	return 200, map[string]any{"delivered": "chat", "chat_reply": answer, "dm": nil,
		"tools_visible": visible}
}

func render(tool string, result map[string]any, callErr error) string {
	if callErr != nil {
		// A tool that reports a problem keeps its own words; the gateway only names the source.
		// This matches the Python reference exactly ("OCR service error: not_a_file"), which is
		// why the parity harness can compare the two replies at all.
		if tool == "ocr_extract" || tool == "ocr_result" {
			return "OCR service error: " + toolErrorText(callErr)
		}
		return "Service error: " + callErr.Error()
	}
	switch tool {
	case "salary_self":
		found, _ := result["found"].(bool)
		if !found {
			return "No salary record found for you."
		}
		return fmt.Sprintf("This month's salary is %s %s.",
			thousand(asInt(result["amount"])), asString(result["currency"]))
	case "docs_search":
		hits, _ := result["hits"].([]any)
		if len(hits) == 0 {
			return "No matching documents found."
		}
		first, _ := hits[0].(map[string]any)
		return "Matching documents: " + asString(first["id"])
	case "ocr_extract":
		id := asString(result["job_id"])
		if id == "" {
			return "Extraction started."
		}
		return fmt.Sprintf("Extraction queued as %s. Ask \"result\" when you want it.", id)
	case "ocr_result":
		switch asString(result["status"]) {
		case "done":
			r, _ := result["result"].(map[string]any)
			chars, pages := asInt(r["chars"]), asInt(r["pages"])
			if chars == 0 {
				return fmt.Sprintf("Extraction finished: %d page(s), no text layer found.", pages)
			}
			return fmt.Sprintf("Extraction finished: %d characters over %d page(s).", chars, pages)
		case "failed":
			return "Extraction failed: " + asString(result["error"])
		case "expired":
			return "That extraction expired; send the document again."
		case "rejected":
			return "The extraction queue is full; try again in a moment."
		default:
			return "Extraction is still running. Ask \"result\" again in a moment."
		}
	case "status":
		return fmt.Sprintf("Status: game=%v, db=%v, redis=%v",
			result["game"], result["db"], result["redis"])
	}
	// A tool the gateway has never heard of still gets a readable line: presentation belongs to
	// the gateway, and requiring a new renderer for every new service is how a boundary rots.
	if hits, ok := result["hits"].([]any); ok {
		if len(hits) == 0 {
			return "No results."
		}
		names := []string{}
		for _, hit := range hits {
			if m, ok := hit.(map[string]any); ok {
				names = append(names, asString(m["id"]))
			}
		}
		return "Result: " + strings.Join(names, ", ")
	}
	if text, ok := result["text"].(string); ok {
		return text
	}
	raw, _ := json.Marshal(result)
	return string(raw)
}

// toolErrorText unwraps the JSON a service returns inside its MCP error, so the caller sees
// "not_a_file" instead of a quoted object.
func toolErrorText(err error) string {
	var payload map[string]any
	if json.Unmarshal([]byte(err.Error()), &payload) == nil {
		if msg := asString(payload["error"]); msg != "" {
			return msg
		}
	}
	return err.Error()
}

func thousand(n int) string {
	s := fmt.Sprint(n)
	out := ""
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out += "."
		}
		out += string(r)
	}
	return out
}

const systemPrompt = "You are a gateway assistant. Use a tool whenever one fits. " +
	"Never invent numbers, names or status: if no tool returns them, say you do not have that information."

// authorizeAdapter fails closed only when a token is configured: an unset token is the
// single-host setup this repo ships, not a silent bypass of a configured one.
func (s *Server) authorizeAdapter(r *http.Request) bool {
	if s.AdapterToken == "" {
		return true
	}
	got := r.Header.Get("X-Adapter-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.AdapterToken)) == 1
}

// Whoami answers, for one caller in one chat, what the model is allowed to see and why each
// hidden tool is hidden. It is the RBAC surface: read-only, no model call, no quota.
func (s *Server) Whoami(userID, chatType string) (int, map[string]any) {
	pal, err := s.Policy.Resolve(userID, chatType)
	if err != nil {
		if d, ok := err.(Denied); ok {
			return http.StatusForbidden, map[string]any{"denied": d.Code, "reason": d.Message}
		}
		return http.StatusForbidden, map[string]any{"denied": "denied", "reason": err.Error()}
	}
	verdicts := s.Policy.Explain(pal, s.Reg.Meta, s.Reg, s.Consent)

	s.mu.Lock()
	used := map[string]int{}
	for k, v := range s.usage[pal.UserID] {
		used[k] = v
	}
	s.mu.Unlock()

	return http.StatusOK, map[string]any{
		"user": map[string]any{
			"id": pal.UserID, "name": pal.Name, "role": pal.Role, "workspace": pal.Workspace,
			"chat_type": pal.ChatType, "personal_data": pal.PersonalOK,
			"free_chat": s.Policy.FreeChat(pal),
		},
		"quota": map[string]any{
			"requests_used": used["requests"], "requests_per_day": s.Policy.Quota.RequestsPerDay,
			"tokens_used": used["tokens"], "tokens_per_day": s.Policy.Quota.TokensPerDay,
		},
		"directory": s.directoryState(pal.UserID),
		"tools":     verdicts,
		"counts":    Counts(verdicts),
	}
}

func (s *Server) directoryState(userID string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	active, known := s.dir[userID]
	return map[string]any{"source": s.dirSource, "loaded": s.dirLoaded, "stale": s.dirStale,
		"known": known, "active": known && active}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	// The console backend: login happens elsewhere, role assignment happens here.
	mux.HandleFunc("/admin/users", s.adminUsers)
	mux.HandleFunc("/admin/users/", s.adminSetRole)
	mux.HandleFunc("/admin/roles", s.adminRoles)
	mux.HandleFunc("/metrics", s.metrics)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("/stats/cache", func(w http.ResponseWriter, r *http.Request) {
		stats := s.Cache.Stats()
		names := []string{}
		for name := range s.Reg.Meta {
			if s.Reg.Advertises(name) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		stats["l3_prefix"] = PrefixFingerprint(names, PolicyVersion)
		stats["tools_advertised"] = names
		writeJSON(w, 200, stats)
	})
	if s.MCP != nil {
		mux.Handle("/mcp", s.MCP)
		mux.HandleFunc("/stats/mcp", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, s.MCP.Stats())
		})
	}
	mux.HandleFunc("/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if !s.authorizeAdapter(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"denied": "bad_adapter_token"})
			return
		}
		code, body := s.Whoami(r.URL.Query().Get("user"), r.URL.Query().Get("chat"))
		writeJSON(w, code, body)
	})
	mux.HandleFunc("/v1/respond", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorizeAdapter(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"denied": "bad_adapter_token"})
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]any{"error": "method_not_allowed"})
			return
		}
		var req respondRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]any{"error": "bad_json"})
			return
		}
		code, body := s.Handle(req)
		writeJSON(w, code, body)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	raw, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	w.Write(raw)
}
