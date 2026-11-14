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
	Roles        map[string]RoleConf `json:"roles"`
	Chats        map[string]ChatConf `json:"chats"`
	Quota        Quota               `json:"quota"`
	// Models is the whitelist for the OpenAI-shaped endpoint. A client may ask for any model
	// name it likes; only the ones named here reach the provider. Empty means nothing is allowed,
	// which is what a new deployment should start with — an endpoint that forwards any model name
	// it is handed is a way to spend somebody else's budget.
	Models ModelConf `json:"models"`
}

// ModelConf narrows what a role may ask for. A role can only shrink `allow`, never extend it:
// the same rule roles follow for tools, for the same reason.
type ModelConf struct {
	Allow  []string            `json:"allow"`
	ByRole map[string][]string `json:"by_role"`
	// Default is what a client that names no model gets. Empty means "refuse", not "pick one".
	Default string `json:"default"`
}

// VisibleModels is the whitelist as this caller sees it, in a stable order.
func (p *Policy) VisibleModels(pal Principal) []string {
	granted := p.Models.Allow
	if role, ok := p.Models.ByRole[pal.Role]; ok {
		for _, name := range role {
			if name == "*" {
				return append([]string{}, granted...)
			}
		}
		granted = intersect(granted, role)
	}
	return append([]string{}, granted...)
}

// ModelAllowed answers the only question the endpoint needs: may this caller spend on this model?
func (p *Policy) ModelAllowed(pal Principal, model string) bool {
	if model == "" {
		model = p.Models.Default
	}
	for _, name := range p.VisibleModels(pal) {
		if name == model {
			return true
		}
	}
	return false
}

func intersect(all, some []string) []string {
	keep := map[string]bool{}
	for _, s := range some {
		keep[s] = true
	}
	out := []string{}
	for _, a := range all {
		if keep[a] {
			out = append(out, a)
		}
	}
	return out
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

// RoleConf is a role's tool grant. `"*"` means "everything the chat already allows": a role can
// only narrow the chat allowlist, never widen it — widening is how a role becomes a backdoor.
type RoleConf struct {
	Tools []string `json:"tools"`
	// Admin marks a role that may assign roles from the console. It is separate from Tools on
	// purpose: seeing every tool is not the same as being allowed to hand out permissions.
	Admin bool `json:"admin"`
}

type Quota struct {
	RequestsPerDay int `json:"requests_per_day"`
	TokensPerDay   int `json:"tokens_per_day"`
	// RequestsPerMinute is the token bucket in front of the daily cliff. 0 disables it, which is
	// the behaviour of every earlier version of this file.
	RequestsPerMinute int `json:"requests_per_minute"`
	Burst             int `json:"burst"`
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
	// `role` used to be parsed and then ignored, which made a user marked "guest" as powerful as
	// an owner. If a roles table exists, every user's role must be in it: an unknown role fails
	// the load instead of silently granting the chat's full allowlist.
	if len(p.Roles) > 0 {
		for id, user := range p.Users {
			if _, ok := p.Roles[user.Role]; !ok {
				return nil, Denied{"policy_invalid",
					"user " + id + " has undefined role: " + user.Role}
			}
		}
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

// roleAllows is a narrowing gate: without a roles table it allows everything (legacy policies),
// with one it allows only what the role names.
func (p *Policy) roleAllows(pal Principal, tool string) bool {
	if len(p.Roles) == 0 {
		return true
	}
	role, ok := p.Roles[pal.Role]
	if !ok {
		return false // fail closed, even if the loader was bypassed
	}
	return contains(role.Tools, "*") || contains(role.Tools, tool)
}

func (p *Policy) maySee(pal Principal, name string, meta map[string]ToolMeta, reg *Registry,
	consent map[string]map[string][]string) bool {
	if !p.roleAllows(pal, name) {
		return false
	}
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
		if !p.roleAllows(pal, name) {
			continue
		}
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

// ConfiguredFor answers "would this tool be allowed here if its service were up?" — the
// distinction that lets a caller tell "not for me" from "broken".
func (p *Policy) ConfiguredFor(pal Principal, tool string, meta map[string]ToolMeta,
	consent map[string]map[string][]string) bool {
	m, known := meta[tool]
	if !known || m.Destructive || !p.roleAllows(pal, tool) {
		return false
	}
	conf := p.chat(pal)
	if !contains(conf.Tools, tool) && !contains(conf.PrivateTools, tool) {
		return false
	}
	return ConsentAllows(pal, m, consent)
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
	if !p.roleAllows(pal, tool) {
		return "", Denied{"tool_not_in_role",
			fmt.Sprintf("role %s may not use %s", pal.Role, tool)}
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
