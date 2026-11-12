package main

import "sort"

// RBAC is not one gate, it is the first gate that says no. Every hidden tool has exactly one
// reason, and the reason is a stable code rather than prose, so a support question
// ("why can't the model see my Drive?") is answered by a query, not by reading four files.
//
// The order below is the documented order: policy facts, destructive, chat, role, discovery,
// consent, personal data. The order does not change the outcome (they are all conjunctions) but
// it does change which reason is reported, and a stable reason is the difference between a
// five-minute answer and a bug report.
const (
	GateNoPolicyFacts   = "no_policy_facts"
	GateDestructive     = "destructive"
	GateNotInChat       = "not_in_chat"
	GateNotInRole       = "not_in_role"
	GateNotAdvertising  = "service_not_advertising"
	GateNoConsent       = "no_consent"
	GatePersonalNotHere = "personal_data_not_here"
)

// ToolVerdict is one tool, one caller, one chat: seen by the model or not, runnable or not, and
// where the answer may be delivered.
type ToolVerdict struct {
	Tool      string `json:"tool"`
	Visible   bool   `json:"visible"`
	Routable  bool   `json:"routable"`
	Delivery  string `json:"delivery,omitempty"` // "chat" or "dm"
	BlockedBy string `json:"blocked_by,omitempty"`
}

// Explain evaluates every tool named either in the policy or in the tool metadata, so a chat
// entry that has no policy facts shows up as no_policy_facts instead of vanishing silently.
func (p *Policy) Explain(pal Principal, meta map[string]ToolMeta, reg *Registry,
	consent map[string]map[string][]string) []ToolVerdict {
	conf := p.chat(pal)
	seen := map[string]bool{}
	for name := range meta {
		seen[name] = true
	}
	for _, name := range conf.Tools {
		seen[name] = true
	}
	for _, name := range conf.PrivateTools {
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ToolVerdict, 0, len(names))
	for _, name := range names {
		m, known := meta[name]
		inChat, inPrivate := contains(conf.Tools, name), contains(conf.PrivateTools, name)
		v := ToolVerdict{Tool: name}

		switch {
		case !known:
			v.BlockedBy = GateNoPolicyFacts
		case m.Destructive:
			v.BlockedBy = GateDestructive
		case !inChat && !inPrivate:
			v.BlockedBy = GateNotInChat
		case !p.roleAllows(pal, name):
			v.BlockedBy = GateNotInRole
		case !reg.Advertises(name):
			v.BlockedBy = GateNotAdvertising
		case !ConsentAllows(pal, m, consent):
			v.BlockedBy = GateNoConsent
		case m.Personal && !pal.PersonalOK:
			// Runnable here, but the answer must leave through a DM: this is the one case where
			// "routable" and "visible" legitimately disagree.
			v.Routable = true
			v.Delivery = "dm"
			v.BlockedBy = GatePersonalNotHere
		default:
			v.Visible, v.Routable, v.Delivery = true, true, "chat"
		}
		out = append(out, v)
	}
	return out
}

// Counts is the one-line summary behind /v1/whoami.
func Counts(vs []ToolVerdict) map[string]int {
	c := map[string]int{"visible": 0, "routable": 0, "hidden": 0}
	for _, v := range vs {
		if v.Visible {
			c["visible"]++
		}
		if v.Routable {
			c["routable"]++
		}
		if !v.Visible {
			c["hidden"]++
		}
	}
	return c
}
