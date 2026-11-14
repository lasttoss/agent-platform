package main

// An OpenAI-shaped model endpoint, so the harness a person already runs — dsh, opencode, any
// client with a base URL and a key — needs exactly one credential: its platform token.
//
// Two rules from the rest of this gateway carry over unchanged:
//
//   - identity arrives in the transport. The bearer token decides who is spending; a user id in
//     the body is ignored, because a caller that can name itself can name somebody else.
//   - the whitelist decides what is reachable. A model name the policy does not name is refused
//     before any provider is asked, so an endpoint cannot be used to reach a model nobody granted.
//
// The provider key never leaves the server: this endpoint holds it, the client holds a token.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type openAIChatRequest struct {
	Model    string           `json:"model"`
	Messages []openAIMessage  `json:"messages"`
	Tools    []map[string]any `json:"tools"`
	Stream   bool             `json:"stream"`
}

// openAIError answers in the shape the OpenAI SDKs parse. A client library that gets a shape it
// does not recognise reports "unknown error" and hides the reason from the person using it.
func openAIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"message": message, "type": "invalid_request_error", "code": code}})
}

// principal is the same identity resolution the MCP front uses: token → user → policy.
func (s *Server) principal(r *http.Request) (Principal, bool) {
	if s.MCP == nil {
		return Principal{}, false
	}
	return s.MCP.whoami(r)
}

func (s *Server) openAIModels(w http.ResponseWriter, r *http.Request) {
	pal, ok := s.principal(r)
	if !ok {
		openAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid or missing token")
		return
	}
	created := time.Now().Unix()
	data := []map[string]any{}
	for _, name := range s.Policy.VisibleModels(pal) {
		data = append(data, map[string]any{
			"id": name, "object": "model", "created": created, "owned_by": "platform"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (s *Server) openAIChat(w http.ResponseWriter, r *http.Request) {
	pal, ok := s.principal(r)
	if !ok {
		openAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid or missing token")
		return
	}
	if ok, retry := s.limiter.allow(pal.UserID); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
		s.audit(map[string]any{"event": "openai_rate_limited", "user": pal.UserID,
			"retry_after_s": retry, "at": time.Now().UTC().Format(time.RFC3339)})
		openAIError(w, http.StatusTooManyRequests, "rate_limited", "retry in "+fmt.Sprint(retry)+"s")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request", "unreadable body")
		return
	}
	var req openAIChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request", "body is not JSON")
		return
	}
	model := req.Model
	if model == "" {
		model = s.Policy.Models.Default
	}
	if !s.Policy.ModelAllowed(pal, model) {
		s.audit(map[string]any{"event": "openai_model_denied", "user": pal.UserID,
			"model": model, "at": time.Now().UTC().Format(time.RFC3339)})
		openAIError(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("model %q is not in this caller's whitelist", model))
		return
	}
	if s.overQuota(pal.UserID) {
		s.audit(map[string]any{"event": "openai_quota_exceeded", "user": pal.UserID,
			"at": time.Now().UTC().Format(time.RFC3339)})
		openAIError(w, http.StatusTooManyRequests, "insufficient_quota", "daily quota reached")
		return
	}
	if len(req.Messages) == 0 {
		openAIError(w, http.StatusBadRequest, "invalid_request", "messages is required")
		return
	}

	system, user := flattenMessages(req.Messages)
	started := time.Now()
	turn, err := s.Reg.Complete(system, user, req.Tools, model)
	if err != nil {
		s.audit(map[string]any{"event": "openai_model_error", "user": pal.UserID, "model": model,
			"error": err.Error(), "at": time.Now().UTC().Format(time.RFC3339)})
		openAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	promptTokens := turn.PromptTokens
	completionTokens := turn.CompletionTokens
	estimated := false
	if promptTokens == 0 && completionTokens == 0 {
		// The MCP model service reports 0 when it answered from a mock; estimate so the counter
		// still moves. Labelled as an estimate everywhere it is reported.
		promptTokens, completionTokens = estimateTokens(system+"\n"+user), estimateTokens(turn.Content)
		estimated = true
	}
	s.chargeRequest(pal.UserID)
	s.chargeTokens(pal.UserID, promptTokens+completionTokens)
	s.audit(map[string]any{"event": "openai_chat", "user": pal.UserID, "role": pal.Role,
		"model": model, "prompt_tokens": promptTokens, "completion_tokens": completionTokens,
		"estimated": estimated, "tool_calls": len(turn.ToolCalls),
		"ms": int(time.Since(started).Milliseconds()), "at": time.Now().UTC().Format(time.RFC3339)})

	id := "chatcmpl-" + randHex(12)
	created := time.Now().Unix()
	if req.Stream {
		streamTurn(w, id, model, created, turn)
		return
	}
	message := map[string]any{"role": "assistant", "content": turn.Content}
	if len(turn.ToolCalls) > 0 {
		message["tool_calls"] = toolCallPayload(turn.ToolCalls)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": finishReason(turn)}},
		"usage": map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completionTokens,
			"total_tokens": promptTokens + completionTokens},
	})
}

// streamTurn emits a valid SSE sequence. The upstream MCP model service answers whole turns, so
// this is one content delta rather than a token stream: a client sees the same events in the same
// order, and the docs say why the chunks are coarse instead of pretending otherwise.
func streamTurn(w http.ResponseWriter, id, model string, created int64, turn ModelTurn) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	send := func(delta map[string]any, finish any) {
		chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created,
			"model": model, "choices": []map[string]any{{"index": 0, "delta": delta,
				"finish_reason": finish}}}
		raw, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send(map[string]any{"role": "assistant"}, nil)
	if len(turn.ToolCalls) > 0 {
		send(map[string]any{"tool_calls": toolCallPayload(turn.ToolCalls)}, nil)
	} else {
		send(map[string]any{"content": turn.Content}, nil)
	}
	send(map[string]any{}, finishReason(turn))
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func finishReason(turn ModelTurn) string {
	if len(turn.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func toolCallPayload(calls []ToolCall) []map[string]any {
	out := []map[string]any{}
	for i, c := range calls {
		args, _ := json.Marshal(c.Args)
		out = append(out, map[string]any{
			"id": fmt.Sprintf("call_%s_%d", randHex(4), i), "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": string(args)}})
	}
	return out
}

// flattenMessages turns a chat history into the two strings the MCP model service takes. The
// service exists to answer one turn from a system prompt and a user message, so a conversation is
// rendered as labelled lines rather than silently dropping turns 2..n.
func flattenMessages(messages []openAIMessage) (system, user string) {
	systems, lines := []string{}, []string{}
	for _, m := range messages {
		text := messageText(m.Content)
		switch m.Role {
		case "system", "developer":
			systems = append(systems, text)
		case "assistant":
			lines = append(lines, "assistant: "+text)
		case "tool":
			lines = append(lines, "tool: "+text)
		default:
			lines = append(lines, text)
		}
	}
	return strings.Join(systems, "\n"), strings.Join(lines, "\n")
}

// messageText accepts both content shapes: a bare string, and the array of parts some clients send.
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err == nil {
		out := []string{}
		for _, p := range parts {
			if asString(p["type"]) == "text" {
				out = append(out, asString(p["text"]))
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
