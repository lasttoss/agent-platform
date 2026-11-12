package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Policy is the only place that decides what a caller may do. Pure: it reads config and
// returns decisions, touches no network. Ported from gateway/policy.py on purpose — one
// policy file, two implementations, and a test suite that runs both.
type Policy struct {
	AllowUnknown bool                `json:"allow_unknown_users"`
	Users        map[string]User     `json:"users"`
	Chats        map[string]ChatConf `json:"chats"`
	Quota        Quota               `json:"quota"`
}

type User struct {
	Name      string `json:"name"`
	Role      string `json:"role"`
	Workspace string `json:"workspace"`
}

type ChatConf struct {
	Tools        []string `json:"tools"`
	PrivateTools []string `json:"private_tools"`
	PersonalData bool     `json:"personal_data"`
	FreeChat     bool     `json:"free_chat"`
}

type Quota struct {
	RequestsPerDay int `json:"requests_per_day"`
	TokensPerDay   int `json:"tokens_per_day"`
}

type Principal struct {
	UserID     string
	Name       string
	Role       string
	Workspace  string
	ChatType   string
	PersonalOK bool
}

type Denied struct {
	Code    string
	Message string
}

func (d Denied) Error() string { return d.Code + ": " + d.Message }

func LoadPolicy(path string) (*Policy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	// Fail closed: a malformed policy must never degrade into "allow".
	if p.Users == nil || p.Chats == nil || p.Quota.RequestsPerDay == 0 {
		return nil, Denied{"policy_invalid", "policy missing users, chats or quota"}
	}
	return &p, nil
}

func (p *Policy) Resolve(userID, chatType string) (Principal, error) {
	user, known := p.Users[userID]
	if !known && !p.AllowUnknown {
		return Principal{}, Denied{"unknown_user", "caller is not provisioned"}
	}
	chat, ok := p.Chats[chatType]
	if !ok {
		return Principal{}, Denied{"unknown_chat", "chat type not allowed: " + chatType}
	}
	return Principal{UserID: userID, Name: user.Name, Role: user.Role,
		Workspace: user.Workspace, ChatType: chatType, PersonalOK: chat.PersonalData}, nil
}

func (p *Policy) chat(pal Principal) ChatConf { return p.Chats[pal.ChatType] }

// VisibleTools is what the model is even told about: filtering happens here, not after a call.
// A tool must clear four gates: named in the chat config, described in tools.json, not
// destructive, and advertised by a running MCP server. Missing any one of them means the
// model never sees it.
// Consent lives inside this function rather than in the caller: "visible" that ignores a
// missing grant is a list somebody can forget to filter a second time.
func (p *Policy) VisibleTools(pal Principal, meta map[string]ToolMeta, reg *Registry,
	consent map[string]map[string][]string) []string {
	out := []string{}
	for _, name := range p.chat(pal).Tools {
		if p.maySee(pal, name, meta, reg, consent) {
			out = append(out, name)
		}
	}
	return out
}

func (p *Policy) maySee(pal Principal, name string, meta map[string]ToolMeta, reg *Registry,
	consent map[string]map[string][]string) bool {
	m, known := meta[name]
	if !known || m.Destructive || !reg.Advertises(name) {
		return false
	}
	if m.Personal && !pal.PersonalOK {
		return false
	}
	return ConsentAllows(pal, m, consent)
}

// RoutableTools is what the gateway may execute: visible tools plus private-only ones
// (personal tools that may run here but must never be posted here).
func (p *Policy) RoutableTools(pal Principal, meta map[string]ToolMeta, reg *Registry,
	consent map[string]map[string][]string) []string {
	conf := p.chat(pal)
	out := []string{}
	for _, name := range append(append([]string{}, conf.Tools...), conf.PrivateTools...) {
		m, known := meta[name]
		// A private tool is exactly the personal one that may RUN here but must not be POSTED
		// here, so the personal check belongs to visibility, not to routability.
		if !known || m.Destructive || !reg.Advertises(name) {
			continue
		}
		if !ConsentAllows(pal, m, consent) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// ConsentAllows: no grant means no tool. Same rule as the chat allowlist — invisible,
// not "fails later".
func ConsentAllows(pal Principal, spec ToolMeta, consent map[string]map[string][]string) bool {
	// Mirrors gateway/policy.py: a provider tool with no required scope needs no grant.
	if spec.Provider == "" || spec.Scope == "" {
		return true
	}
	granted := consent[pal.UserID][spec.Provider]
	for _, s := range granted {
		if s == spec.Scope {
			return true
		}
	}
	return false
}

func (p *Policy) FreeChat(pal Principal) bool { return p.chat(pal).FreeChat }

// Decide returns "call" or "private".
func (p *Policy) Decide(pal Principal, tool string, spec ToolMeta, known bool) (string, error) {
	if !known {
		return "", Denied{"unknown_tool", "no such tool: " + tool}
	}
	if spec.Destructive {
		return "", Denied{"destructive_blocked", tool + " is not allowed from chat"}
	}
	conf := p.chat(pal)
	inChat, inPrivate := contains(conf.Tools, tool), contains(conf.PrivateTools, tool)
	if inPrivate && !spec.Personal {
		return "", Denied{"policy_invalid", tool + " is private but not personal"}
	}
	if !inChat && !inPrivate {
		return "", Denied{"tool_not_in_chat", fmt.Sprintf("%s is not enabled for %s", tool, pal.ChatType)}
	}
	if spec.Personal && !pal.PersonalOK {
		return "private", nil
	}
	return "call", nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// LoadConsent reads consent.json: {userID: {provider: [scopes]}}.
func LoadConsent(path string) (map[string]map[string][]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]map[string][]string{}, nil
		}
		return nil, err
	}
	out := map[string]map[string][]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ApplyLocal merges gitignored local overrides: real ids and consent never live in git.
func ApplyLocal(p *Policy, consent map[string]map[string][]string, local LocalFile) {
	for id, u := range local.Users {
		p.Users[id] = u
	}
	for id, providers := range local.Consent {
		if consent[id] == nil {
			consent[id] = map[string][]string{}
		}
		for provider, scopes := range providers {
			consent[id][provider] = scopes
		}
	}
}

// LocalFile is the gitignored overlay: real ids, real grants. Salary is read by the demo tool
// service, not by the gateway — the gateway never holds an amount.
type LocalFile struct {
	Users   map[string]User                `json:"users"`
	Consent map[string]map[string][]string `json:"consent"`
	Salary  map[string]SalaryRecord        `json:"salary"`
}

func LoadLocal(path string) (LocalFile, error) {
	var l LocalFile
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return l, err
	}
	return l, json.Unmarshal(raw, &l)
}
