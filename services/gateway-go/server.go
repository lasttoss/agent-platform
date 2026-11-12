package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
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
}

var pathRE = regexp.MustCompile(`[\w./-]+\.(?:pdf|png|jpe?g|tiff?|bmp|webp)`)

type Server struct {
	Policy  *Policy
	Consent map[string]map[string][]string
	Reg     *Registry
	Cache   *Cache
	Audit   string

	mu    sync.Mutex
	usage map[string]map[string]int // userID → {"requests", "tokens"}
}

func NewServer(p *Policy, consent map[string]map[string][]string, reg *Registry, auditPath string) *Server {
	return &Server{Policy: p, Consent: consent, Reg: reg, Cache: NewCache(), Audit: auditPath,
		usage: map[string]map[string]int{}}
}

type respondRequest struct {
	PlatformUserID string `json:"platform_user_id"`
	ChatType       string `json:"chat_type"`
	Text           string `json:"text"`
}

func (s *Server) audit(rec map[string]any) {
	if s.Audit == "" {
		return
	}
	rec["ts"] = time.Now().Format(time.RFC3339)
	raw, _ := json.Marshal(rec)
	f, err := os.OpenFile(s.Audit, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(raw, '\n'))
}

func (s *Server) charge(userID string, tokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usage[userID] == nil {
		s.usage[userID] = map[string]int{}
	}
	s.usage[userID]["requests"]++
	s.usage[userID]["tokens"] += tokens
}

func (s *Server) overQuota(userID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.usage[userID]
	return u["requests"] >= s.Policy.Quota.RequestsPerDay || u["tokens"] >= s.Policy.Quota.TokensPerDay
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

	visible := s.Policy.VisibleTools(pal, s.Reg.Meta, s.Reg, s.Consent)
	routable := s.Policy.RoutableTools(pal, s.Reg.Meta, s.Reg, s.Consent)

	tool, args, matched := pickTool(req.Text)
	ttl := time.Duration(0)
	if matched {
		ttl = s.ttl(tool)
	}
	scope := scopeFor(pal, tool, s.Reg.Meta)
	if matched && !contains(routable, tool) {
		matched = false
	}
	key := ExactKey(req.Text, visible, PolicyVersion, pal.ChatType, scope)
	if ttl > 0 {
		if hit, ok := s.Cache.GetExact(key); ok {
			s.charge(pal.UserID, 0)
			s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
				"decision": "cache_exact", "scope": scope, "tool": tool})
			return 200, map[string]any{"delivered": "chat", "chat_reply": hit.Answer,
				"dm": nil, "tools_visible": visible,
				"cache": map[string]any{"layer": "exact", "saved_tokens": hit.Saved}}
		}
		if hit, sim, ok := s.Cache.GetSemantic(req.Text, scope); ok {
			s.charge(pal.UserID, 0)
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
			s.charge(pal.UserID, turn.PromptTokens+turn.CompletionTokens)
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
						"chat_reply":  "Mình không có quyền làm việc này.",
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
		s.charge(pal.UserID, 0)
		s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
			"decision": "no_tool", "tools_visible": visible})
		return 200, map[string]any{"delivered": "chat",
			"chat_reply": "Mình chưa có công cụ cho việc này.", "dm": nil,
			"tools_visible": visible}
	}

	meta, known := s.Reg.Meta[tool]
	mode, err := s.Policy.Decide(pal, tool, meta, known)
	if err != nil {
		d := err.(Denied)
		s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
			"decision": "denied", "code": d.Code, "tool": tool})
		return 403, map[string]any{"denied": map[string]any{"code": d.Code, "reason": d.Message}}
	}

	// The gateway executes nothing itself: one MCP call, same shape for every capability.
	result, callErr := s.Reg.CallTool(tool, args, pal.UserID)
	answer := render(tool, result, callErr)
	s.charge(pal.UserID, 0)
	if ttl > 0 && callErr == nil {
		s.Cache.PutExact(key, CacheEntry{Answer: answer, TTL: ttl})
		s.Cache.PutSemantic(req.Text, answer, 0, ttl, scope)
	}

	if mode == "private" {
		s.audit(map[string]any{"user": pal.UserID, "chat": pal.ChatType,
			"decision": "private_delivery", "tool": tool})
		return 200, map[string]any{"delivered": "dm",
			"chat_reply":    "Mình gửi riêng cho bạn nhé.",
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
		return "Dịch vụ trả lỗi: " + callErr.Error()
	}
	switch tool {
	case "salary_self":
		found, _ := result["found"].(bool)
		if !found {
			return "Không tìm thấy bản ghi lương của bạn."
		}
		return fmt.Sprintf("Lương tháng này của bạn là %s %s.",
			thousand(asInt(result["amount"])), asString(result["currency"]))
	case "docs_search":
		hits, _ := result["hits"].([]any)
		if len(hits) == 0 {
			return "Không tìm thấy tài liệu phù hợp."
		}
		first, _ := hits[0].(map[string]any)
		return "Tài liệu liên quan: " + asString(first["id"])
	case "status":
		return fmt.Sprintf("Trạng thái: game=%v, db=%v, redis=%v",
			result["game"], result["db"], result["redis"])
	}
	if text, ok := result["text"].(string); ok {
		return text
	}
	return fmt.Sprint(result)
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

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
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
	mux.HandleFunc("/v1/respond", func(w http.ResponseWriter, r *http.Request) {
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
