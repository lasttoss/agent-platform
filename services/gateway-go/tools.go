package main

import "fmt"

// Tool spec: the same registry the policy reads, so a tool cannot exist without a stated
// interface, and the model can only be offered parameters we actually parse.
type ToolSpec struct {
	Personal    bool
	Destructive bool
	Provider    string
	Scope       string
	Description string
}

var Tools = map[string]ToolSpec{
	"salary_self": {Personal: true, Description: "Return the salary record of the caller."},
	"docs_search": {Description: "Search internal documents."},
	"status":      {Description: "Return service health."},
	"ocr_extract": {Personal: true, Provider: "local_ocr", Description: "Extract text from a document."},
	"drive_search": {Personal: true, Provider: "google_drive", Scope: "drive.readonly",
		Description: "Search the caller's own Drive."},
	"jira_search": {Provider: "jira", Scope: "read:jira-work", Description: "Search Jira issues."},
	"mail_list": {Personal: true, Provider: "imap", Scope: "mail.read",
		Description: "List recent mail for the caller."},
	"deploy_prod": {Destructive: true, Description: "Deploy to production. Never exposed to chat."},
}

type SalaryRecord struct {
	Amount   int    `json:"amount"`
	Currency string `json:"currency"`
}

// Demo fixtures. Counting-pattern digits, never a plausible salary.
var Salary = map[string]SalaryRecord{
	"1001": {Amount: 12_345_000, Currency: "VND"},
	"2002": {Amount: 9_876_000, Currency: "VND"},
}

type ToolCall struct {
	Name string
	Args map[string]any
}

func Schema(name string) map[string]any {
	spec, ok := Tools[name]
	if !ok {
		return nil
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": spec.Description,
			"parameters":  ParamsFor(name),
		},
	}
}

func ParamsFor(name string) map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		p := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			p["required"] = required
		}
		return p
	}
	switch name {
	case "docs_search", "drive_search", "jira_search":
		return obj(map[string]any{"q": map[string]any{"type": "string"}}, "q")
	case "ocr_extract":
		return obj(map[string]any{"path": map[string]any{"type": "string"}}, "path")
	default:
		return obj(map[string]any{})
	}
}

func CallTool(name string, args map[string]any, principal Principal) (map[string]any, error) {
	if name == "salary_self" {
		// No user argument on purpose: the caller cannot ask for someone else's record.
		rec, ok := Salary[principal.UserID]
		if !ok {
			return map[string]any{"found": false}, nil
		}
		return map[string]any{"found": true, "amount": rec.Amount, "currency": rec.Currency}, nil
	}
	if name == "docs_search" {
		q := Fold(fmt.Sprint(args["q"]))
		hits := []map[string]any{}
		for _, d := range Docs {
			if containsFold(d.title, q) || containsFold(d.id, q) || q == "" {
				hits = append(hits, map[string]any{"id": d.id, "title": d.title})
			}
		}
		if len(hits) > 3 {
			hits = hits[:3]
		}
		return map[string]any{"query": q, "hits": hits}, nil
	}
	if name == "status" {
		return map[string]any{"game": "ok", "db": "ok", "redis": "ok"}, nil
	}
	return map[string]any{"error": "not_implemented_in_go", "tool": name}, nil
}

type doc struct{ id, title string }

var Docs = []doc{
	{"runbook-restart", "How to restart the game server without dropping sessions"},
	{"policy-data", "What counts as personal data in our systems"},
	{"oncall", "On-call rota and escalation path"},
}

func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return len(Fold(haystack)) > 0 && indexOf(Fold(haystack), Fold(needle)) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
