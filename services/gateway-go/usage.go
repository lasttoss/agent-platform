package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Usage has to survive a restart, or "requests per day" is really "requests per process", and a
// deploy at noon hands everybody a second allowance. This file is a small, honest store: JSON on
// disk, flushed by a background writer, and single-process. It is not a distributed counter —
// see the note in the README before running two gateways against one file.

type usageFile struct {
	Day     string                    `json:"day"`
	Users   map[string]map[string]int `json:"users"`
	SavedAt string                    `json:"saved_at"`
}

// usageBackend is what the server needs from a counter store: load today, save deltas, and keep
// doing both while the process runs. Two implementations exist: a JSON file for one process, and
// Postgres for a deployment with replicas.
type usageBackend interface {
	load(*Server) (bool, error)
	save(*Server) error
	mark()
	run(*Server, time.Duration)
}

type usageStore struct {
	path  string
	mu    sync.Mutex
	dirty chan struct{}
}

func newUsageStore(path string) *usageStore {
	return &usageStore{path: path, dirty: make(chan struct{}, 1)}
}

// load merges today's counters back into the server. A file from another day is ignored: the
// quota is per day, and resurrecting yesterday's numbers would refuse people for no reason.
func (u *usageStore) load(s *Server) (bool, error) {
	raw, err := os.ReadFile(u.path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var f usageFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return false, err
	}
	if f.Day != today() {
		return false, nil
	}
	s.mu.Lock()
	for user, counts := range f.Users {
		s.usage[user] = counts
	}
	s.mu.Unlock()
	return true, nil
}

func (u *usageStore) save(s *Server) error {
	s.mu.Lock()
	out := usageFile{Day: today(), Users: map[string]map[string]int{},
		SavedAt: time.Now().UTC().Format(time.RFC3339)}
	// Copy: the writer must not hold the server lock while encoding.
	users := make([]string, 0, len(s.usage))
	for user := range s.usage {
		users = append(users, user)
	}
	for _, user := range users {
		cp := map[string]int{}
		for k, v := range s.usage[user] {
			cp[k] = v
		}
		out.Users[user] = cp
	}
	s.mu.Unlock()

	sort.Strings(users)
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(u.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(u.path), ".usage-*")
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
	return os.Rename(name, u.path)
}

// mark schedules a save. Coalescing is the point: a burst of a thousand requests writes twice.
func (u *usageStore) mark() {
	select {
	case u.dirty <- struct{}{}:
	default:
	}
}

// run writes at most once per interval while marks keep arriving. Called as a goroutine.
func (u *usageStore) run(s *Server, every time.Duration) {
	for range u.dirty {
		time.Sleep(every)
		if err := u.save(s); err != nil {
			// Losing the counter is bad but not fatal: the process still enforces the quota it
			// remembers, and the error is visible instead of silent.
			auditLine := map[string]any{"event": "usage_save_failed", "error": err.Error(),
				"at": time.Now().UTC().Format(time.RFC3339)}
			s.audit(auditLine)
		}
		// Drain marks accumulated during the sleep so the next save is for them, not for nothing.
		select {
		case <-u.dirty:
		default:
		}
	}
}

func today() string { return time.Now().UTC().Format("2006-01-02") }
