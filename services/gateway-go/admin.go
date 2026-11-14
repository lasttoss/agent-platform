package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The console backend. Login is the company's business (token file today, SSO later); this file
// starts where login ends: somebody has been authenticated, and an administrator wants to say
// what that person may do. Four rules, all enforced here rather than in a UI:
//
//  1. only an admin role may assign roles;
//  2. nobody changes their own role (a mis-click must not demote the only admin);
//  3. the last admin cannot be demoted;
//  4. every change is audited with actor, subject, from and to, before the new state is served.

func (s *Server) adminCaller(r *http.Request) (Principal, bool) {
	if s.MCP == nil {
		return Principal{}, false
	}
	return s.MCP.whoami(r)
}

func (s *Server) adminGuard(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	pal, ok := s.adminCaller(r)
	if !ok {
		writeAdmin(w, http.StatusUnauthorized, map[string]any{"error": "unknown token"})
		return Principal{}, false
	}
	if !s.Policy.Roles[pal.Role].Admin {
		s.audit(map[string]any{"event": "admin_denied", "user": pal.UserID, "role": pal.Role,
			"path": r.URL.Path, "at": time.Now().UTC().Format(time.RFC3339)})
		writeAdmin(w, http.StatusForbidden, map[string]any{"error": "role " + pal.Role + " is not admin"})
		return Principal{}, false
	}
	return pal, true
}

// adminConsole serves the page itself. It carries no data and holds no session: the token lives
// in the tab, so an unauthenticated GET of this file reveals nothing.
func (s *Server) adminConsole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdmin(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	w.Write([]byte(adminConsoleHTML))
}

func writeAdmin(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

// subjects is every person the platform knows: the policy's users plus anyone the console has
// assigned. Sorted, because a console that reorders itself is a console people mis-click in.
func (s *Server) subjects() []string {
	seen := map[string]bool{}
	for id := range s.Policy.Users {
		seen[id] = true
	}
	if s.Roles != nil {
		for id := range s.Roles.Snapshot() {
			seen[id] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdmin(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
		return
	}
	if _, ok := s.adminGuard(w, r); !ok {
		return
	}
	type row struct {
		Subject  string `json:"subject"`
		Name     string `json:"name,omitempty"`
		Role     string `json:"role"`
		InPolicy bool   `json:"in_policy"`
		Source   string `json:"source"`
	}
	out := make([]row, 0)
	for _, id := range s.subjects() {
		role, known := s.effectiveRole(id)
		source := "policy"
		if s.Roles != nil {
			if _, ok := s.Roles.Lookup(id); ok {
				source = "console"
			}
		}
		out = append(out, row{Subject: id, Name: s.Policy.Users[id].Name, Role: role,
			InPolicy: known, Source: source})
	}
	writeAdmin(w, http.StatusOK, map[string]any{"users": out, "generation": s.policyGeneration()})
}

func (s *Server) adminRoles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdmin(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
		return
	}
	if _, ok := s.adminGuard(w, r); !ok {
		return
	}
	type row struct {
		Role  string   `json:"role"`
		Tools []string `json:"tools"`
		Admin bool     `json:"admin"`
	}
	names := make([]string, 0, len(s.Policy.Roles))
	for name := range s.Policy.Roles {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]row, 0, len(names))
	for _, name := range names {
		rc := s.Policy.Roles[name]
		tools := rc.Tools
		if tools == nil {
			tools = []string{}
		}
		out = append(out, row{Role: name, Tools: tools, Admin: rc.Admin})
	}
	writeAdmin(w, http.StatusOK, map[string]any{"roles": out})
}

// adminCount counts subjects whose effective role may assign roles. Used to refuse the change
// that would leave nobody able to administer the platform.
func (s *Server) adminCount() int {
	n := 0
	for _, id := range s.subjects() {
		if role, ok := s.effectiveRole(id); ok && s.Policy.Roles[role].Admin {
			n++
		}
	}
	return n
}

func (s *Server) adminSetRole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdmin(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	pal, ok := s.adminGuard(w, r)
	if !ok {
		return
	}
	// /admin/users/{subject}/role
	rest := strings.TrimPrefix(r.URL.Path, "/admin/users/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "role" {
		writeAdmin(w, http.StatusNotFound, map[string]any{"error": "want /admin/users/{subject}/role"})
		return
	}
	subject := parts[0]
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeAdmin(w, http.StatusBadRequest, map[string]any{"error": "body must be {\"role\": \"...\"}"})
		return
	}
	newRole := strings.TrimSpace(body.Role)
	if _, defined := s.Policy.Roles[newRole]; !defined {
		writeAdmin(w, http.StatusBadRequest, map[string]any{"error": "unknown role: " + newRole})
		return
	}
	if subject == pal.UserID {
		writeAdmin(w, http.StatusConflict,
			map[string]any{"error": "refusing to change your own role; ask another admin"})
		return
	}
	old, known := s.effectiveRole(subject)
	if !known {
		writeAdmin(w, http.StatusNotFound, map[string]any{"error": "unknown subject: " + subject})
		return
	}
	if s.Policy.Roles[old].Admin && !s.Policy.Roles[newRole].Admin && s.adminCount() <= 1 {
		writeAdmin(w, http.StatusConflict,
			map[string]any{"error": "refusing to remove the last admin"})
		return
	}
	if s.Roles == nil {
		writeAdmin(w, http.StatusServiceUnavailable,
			map[string]any{"error": "role store not configured (start with -roles)"})
		return
	}
	if err := s.Roles.Assign(subject, newRole); err != nil {
		writeAdmin(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.audit(map[string]any{"event": "admin_role_change", "actor": pal.UserID, "actor_role": pal.Role,
		"subject": subject, "from": old, "to": newRole, "generation": s.policyGeneration(),
		"at": time.Now().UTC().Format(time.RFC3339)})
	writeAdmin(w, http.StatusOK, map[string]any{"subject": subject, "from": old, "to": newRole,
		"generation": s.policyGeneration()})
}
