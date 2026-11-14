package main

// Token rotation without a restart. The file is the source of truth; every request checks its
// mtime, so an operator replaces the file and the next call uses the new tokens. A token that
// disappears is refused on the next request — there is no window where a removed token still
// works because "the process had it cached".

import (
	"crypto/subtle"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type TokenMap struct {
	// token → user id in policy.json.
	Tokens map[string]string `json:"tokens"`
}

type tokenSet struct {
	path string

	mu     sync.RWMutex
	mtime  time.Time
	tokens map[string]string
}

type rotateReport struct {
	Added   []string
	Removed []string
}

func readTokenFile(path string) (map[string]string, time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	var tm TokenMap
	if err := json.Unmarshal(raw, &tm); err != nil {
		return nil, time.Time{}, err
	}
	if tm.Tokens == nil {
		tm.Tokens = map[string]string{}
	}
	return tm.Tokens, info.ModTime(), nil
}

func NewTokenSet(path string) (*tokenSet, error) {
	tokens, mtime, err := readTokenFile(path)
	if err != nil {
		return nil, err
	}
	return &tokenSet{path: path, mtime: mtime, tokens: tokens}, nil
}

// Save writes the whole map back, atomically, then adopts it as the current state.
//
// Write-temp-then-rename because every request reloads on mtime: a half-written file would be
// read as "the operator deleted tokens" and lock people out for the duration of the write. The
// in-memory map is replaced from the same data, so the next request in this process is authorized
// by the new file rather than by the old map waiting for a mtime tick.
func (t *tokenSet) Save(tokens map[string]string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	raw, err := json.MarshalIndent(TokenMap{Tokens: tokens}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(t.path)
	tmp, err := os.CreateTemp(dir, ".tokens-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, t.path); err != nil {
		return err
	}
	if info, err := os.Stat(t.path); err == nil {
		t.mtime = info.ModTime()
	}
	t.tokens = tokens
	return nil
}

// snapshot is a copy of the current map, for a caller that intends to modify and save it.
func (t *tokenSet) snapshot() map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]string, len(t.tokens))
	for k, v := range t.tokens {
		out[k] = v
	}
	return out
}

// userFor reports which user a token is for, without revealing the token.
func (t *tokenSet) userFor(tok string) (string, bool) {
	uid, ok, _ := t.Lookup(tok)
	return uid, ok
}

func (t *tokenSet) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.tokens)
}

// reload swaps the map if the file changed. The diff is returned, not logged here: the caller
// owns the audit trail.
func (t *tokenSet) reload() rotateReport {
	fresh, mtime, err := readTokenFile(t.path)
	if err != nil || !mtime.After(t.mtime) {
		return rotateReport{}
	}
	t.mu.Lock()
	old := t.tokens
	t.tokens, t.mtime = fresh, mtime
	t.mu.Unlock()

	rep := rotateReport{}
	for tok, user := range fresh {
		if prev, ok := old[tok]; !ok || prev != user {
			rep.Added = append(rep.Added, maskToken(tok))
		}
	}
	for tok := range old {
		if _, ok := fresh[tok]; !ok {
			rep.Removed = append(rep.Removed, maskToken(tok))
		}
	}
	return rep
}

// Lookup resolves a token to a user id, refreshing the file first when it changed on disk.
func (t *tokenSet) Lookup(tok string) (string, bool, rotateReport) {
	rep := t.reload()
	t.mu.RLock()
	defer t.mu.RUnlock()
	for known, userID := range t.tokens {
		if subtle.ConstantTimeCompare([]byte(known), []byte(tok)) == 1 {
			return userID, true, rep
		}
	}
	return "", false, rep
}

// maskToken keeps a token recognisable in an audit line without reproducing it: a leaked audit
// log must not become a leaked credential.
func maskToken(tok string) string {
	if len(tok) <= 8 {
		return "****"
	}
	return tok[:4] + "…" + tok[len(tok)-4:]
}
