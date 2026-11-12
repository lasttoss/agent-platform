// Package demotools is the reference tool service: three tools, no dependencies, MCP only.
// Both the binary in cmd/mcp-demo and the gateway tests use it, so the fixtures can never
// drift between "the demo" and "the thing the tests believe".
package demotools

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/lasttoss/agent-platform/services/gateway-go/internal/fold"
	"github.com/lasttoss/agent-platform/services/gateway-go/internal/mcpserver"
)

// Counting-pattern digits on purpose: "a plausible salary" is how real payroll data ends up
// in a public repo.
var Salary = map[string]map[string]any{
	"1001": {"amount": 12_345_000, "currency": "VND"},
	"2002": {"amount": 9_876_000, "currency": "VND"},
}

type doc struct{ id, title string }

var docs = []doc{
	{"runbook-restart", "How to restart the game server without dropping sessions"},
	{"policy-data", "What counts as personal data in our systems"},
	{"oncall", "On-call rota and escalation path"},
}

func Handler() *mcpserver.Handler {
	return &mcpserver.Handler{Name: "demo-tools", Version: "1.0.0", Call: Call,
		Tools: []mcpserver.Tool{
			{Name: "salary_self", Description: "Return the salary record of the caller.",
				InputSchema: object(nil)},
			{Name: "docs_search", Description: "Search internal documents.",
				InputSchema: object(map[string]any{"q": map[string]any{"type": "string"}}, "q")},
			{Name: "status", Description: "Return service health.", InputSchema: object(nil)},
		}}
}

// Configure makes this service behave like a real downstream: a bounded worker pool and a call
// that takes time. Without it, every benchmark measures an imaginary service that is infinitely
// fast and infinitely parallel — which is exactly the assumption goroutines tempt you into.
func Configure(workers int, delay time.Duration) {
	Delay = delay
	if workers > 0 {
		bounded = make(chan struct{}, workers)
	} else {
		bounded = nil
	}
}

var (
	Delay   time.Duration
	bounded chan struct{}
)

func Call(name string, args map[string]any, r *http.Request) (any, error) {
	if bounded != nil {
		bounded <- struct{}{}
		defer func() { <-bounded }()
	}
	if Delay > 0 {
		time.Sleep(Delay)
	}
	return call(name, args, r)
}

func call(name string, args map[string]any, r *http.Request) (any, error) {
	switch name {
	case "salary_self":
		// The caller's id comes from transport, never from a tool argument:
		// a model must not be able to name whose salary it wants.
		rec, ok := Salary[r.Header.Get("X-Caller-Id")]
		if !ok {
			return map[string]any{"found": false}, nil
		}
		return map[string]any{"found": true, "amount": rec["amount"], "currency": rec["currency"]}, nil
	case "docs_search":
		q := fold.Fold(stringOf(args["q"]))
		hits := []map[string]any{}
		for _, d := range docs {
			if q == "" || containsFold(d.id, q) || containsFold(d.title, q) {
				hits = append(hits, map[string]any{"id": d.id, "title": d.title})
			}
		}
		if len(hits) > 3 {
			hits = hits[:3]
		}
		return map[string]any{"query": q, "hits": hits}, nil
	case "status":
		return map[string]any{"game": "ok", "db": "ok", "redis": "ok"}, nil
	}
	return nil, nil
}

// LoadLocal applies gitignored salary overrides ({salary:{user:{amount,currency}}}).
func LoadLocal(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc struct {
		Salary map[string]struct {
			Amount   int    `json:"amount"`
			Currency string `json:"currency"`
		} `json:"salary"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	for id, rec := range doc.Salary {
		Salary[id] = map[string]any{"amount": rec.Amount, "currency": rec.Currency}
	}
	return nil
}

func object(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	obj := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		obj["required"] = required
	}
	return obj
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func containsFold(hay, needle string) bool {
	h, n := fold.Fold(hay), fold.Fold(needle)
	if n == "" {
		return true
	}
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
