package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// roleStore is where a console writes "this person is now a dev". It is deliberately separate
// from policy.json: a policy is a file an engineer reviews in a pull request, a role assignment
// is a decision an administrator makes at 14:00 on a Tuesday, and the second one must not
// require a deploy.
//
// Every load that actually changes something bumps the generation. Cache keys include it, so an
// assignment takes effect on the next request instead of whenever an entry happens to expire —
// "the console said the permission is gone" and "the data is still coming back" cannot both be
// true.
type roleStore struct {
	path string

	mu    sync.RWMutex
	mtime time.Time
	roles map[string]string // subject → role
	gen   int
}

type roleFile struct {
	Roles map[string]string `json:"roles"`
}

// NewRoleStore reads the file once. A missing file is not an error: it means nobody has been
// assigned a role yet, and policy.json still decides.
func NewRoleStore(path string) (*roleStore, error) {
	rs := &roleStore{path: path, roles: map[string]string{}, gen: 1}
	if err := rs.reloadLocked(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return rs, nil
}

func (rs *roleStore) read() (map[string]string, time.Time, error) {
	raw, err := os.ReadFile(rs.path)
	if err != nil {
		return nil, time.Time{}, err
	}
	st, err := os.Stat(rs.path)
	if err != nil {
		return nil, time.Time{}, err
	}
	var f roleFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, time.Time{}, err
	}
	if f.Roles == nil {
		f.Roles = map[string]string{}
	}
	return f.Roles, st.ModTime(), nil
}

func (rs *roleStore) reloadLocked() error {
	roles, mtime, err := rs.read()
	if err != nil {
		return err
	}
	if mtime.Equal(rs.mtime) {
		return nil
	}
	rs.mtime = mtime
	if !sameRoles(rs.roles, roles) {
		rs.roles = roles
		rs.gen++
	}
	return nil
}

func sameRoles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// reload picks up an edit made by hand, the same way the token file does.
func (rs *roleStore) reload() (changed bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	before := rs.gen
	rs.reloadLocked()
	return rs.gen != before
}

func (rs *roleStore) Lookup(subject string) (string, bool) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	r, ok := rs.roles[subject]
	return r, ok
}

func (rs *roleStore) Generation() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.gen
}

func (rs *roleStore) Snapshot() map[string]string {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	out := make(map[string]string, len(rs.roles))
	for k, v := range rs.roles {
		out[k] = v
	}
	return out
}

// Assign writes the new map atomically (tmp + rename) and only then bumps the generation. A
// crash between the two leaves the old file, which is the safe direction to be wrong in.
func (rs *roleStore) Assign(subject, role string) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	next := make(map[string]string, len(rs.roles)+1)
	for k, v := range rs.roles {
		next[k] = v
	}
	next[subject] = role
	raw, err := json.MarshalIndent(roleFile{Roles: next}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(rs.path)
	tmp, err := os.CreateTemp(dir, ".roles-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	tmp.Close()
	if err := os.Rename(name, rs.path); err != nil {
		os.Remove(name)
		return err
	}
	rs.roles = next
	rs.gen++
	if st, err := os.Stat(rs.path); err == nil {
		rs.mtime = st.ModTime()
	}
	return nil
}

// effectiveRole answers "what is this person now": the console's answer if there is one,
// otherwise the policy file's. A store entry naming a role the policy does not define is
// ignored rather than trusted — the policy is still the only place that defines what a role can do.
func (s *Server) effectiveRole(subject string) (string, bool) {
	fallback, known := "", false
	if u, ok := s.Policy.Users[subject]; ok {
		fallback, known = u.Role, true
	}
	if s.Roles == nil {
		return fallback, known
	}
	r, ok := s.Roles.Lookup(subject)
	if !ok {
		return fallback, known
	}
	if _, defined := s.Policy.Roles[r]; !defined {
		return fallback, known
	}
	return r, true
}

func (s *Server) policyGeneration() int {
	if s.Roles == nil {
		return 0
	}
	return s.Roles.Generation()
}
