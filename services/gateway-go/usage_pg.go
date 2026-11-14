package main

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

// Shared counters, so two gateway replicas do not each hand out the full daily allowance.
//
// The local map is still the thing quota decisions read: a database round-trip in front of every
// call would make the quota the slowest part of the gateway. Instead each process writes only the
// *delta* it has not reported yet, every half second, and periodically reloads the rows:
//
//	effective(user) = db(user) + (local(user) - reported(user))
//
// Summing deltas is what makes two replicas add up instead of overwriting each other. What it does
// not give is an exact instant: between writes, two processes can each spend a little of the same
// allowance. That window is stated here and in the docs rather than hidden.

type pgUsage struct {
	db *sql.DB

	mu       sync.Mutex
	reported map[string]map[string]int // per user: what this process already wrote
	dirty    chan struct{}
}

const usageSchema = `
CREATE TABLE IF NOT EXISTS usage_counters (
    day        date        NOT NULL,
    user_id    text        NOT NULL,
    requests   bigint      NOT NULL DEFAULT 0,
    tokens     bigint      NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (day, user_id)
)`

func newPgUsage(dsn string) (*pgUsage, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	if _, err := db.Exec(usageSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("usage table: %w", err)
	}
	return &pgUsage{db: db, reported: map[string]map[string]int{}, dirty: make(chan struct{}, 1)}, nil
}

func (p *pgUsage) mark() {
	select {
	case p.dirty <- struct{}{}:
	default:
	}
}

// load merges the database into the local counters, keeping the deltas this process has not
// written yet. Returns whether today already has rows.
func (p *pgUsage) load(s *Server) (bool, error) {
	rows, err := p.db.Query(`SELECT user_id, requests, tokens FROM usage_counters WHERE day = $1`, today())
	if err != nil {
		return false, err
	}
	defer rows.Close()
	stored := map[string]map[string]int{}
	for rows.Next() {
		var user string
		var req, tok int64
		if err := rows.Scan(&user, &req, &tok); err != nil {
			return false, err
		}
		stored[user] = map[string]int{"requests": int(req), "tokens": int(tok)}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for user, local := range s.usage {
		rep := p.reported[user]
		base := stored[user]
		if base == nil {
			base = map[string]int{}
		}
		merged := map[string]int{
			"requests": base["requests"] + maxInt(0, local["requests"]-rep["requests"]),
			"tokens":   base["tokens"] + maxInt(0, local["tokens"]-rep["tokens"]),
		}
		s.usage[user] = merged
		// What the database is known to hold. Without this line a restart counted everything
		// twice: load() folded the stored numbers into the local map while `reported` stayed
		// empty, so the next flush wrote them again as if they were new spending.
		p.reported[user] = map[string]int{
			"requests": maxInt(base["requests"], rep["requests"]),
			"tokens":   maxInt(base["tokens"], rep["tokens"]),
		}
	}
	for user, base := range stored {
		if _, local := s.usage[user]; !local {
			s.usage[user] = map[string]int{"requests": base["requests"], "tokens": base["tokens"]}
			// Same reason as above: what came from the database is already stored, so it must
			// never be written back as new spending.
			p.reported[user] = map[string]int{"requests": base["requests"], "tokens": base["tokens"]}
		}
	}
	return len(stored) > 0, nil
}

// save writes only what this process has not written yet, as increments.
func (p *pgUsage) save(s *Server) error {
	type delta struct {
		user               string
		req, tok           int
		reqAfter, tokAfter int
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var deltas []delta
	s.mu.Lock()
	for user, local := range s.usage {
		rep := p.reported[user]
		req, tok := local["requests"]-rep["requests"], local["tokens"]-rep["tokens"]
		if req < 0 {
			req = 0
		}
		if tok < 0 {
			tok = 0
		}
		if req == 0 && tok == 0 {
			continue
		}
		deltas = append(deltas, delta{user: user, req: req, tok: tok,
			reqAfter: local["requests"], tokAfter: local["tokens"]})
	}
	s.mu.Unlock()

	for _, d := range deltas {
		_, err := p.db.Exec(`INSERT INTO usage_counters (day, user_id, requests, tokens)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (day, user_id) DO UPDATE
			SET requests = usage_counters.requests + EXCLUDED.requests,
			    tokens   = usage_counters.tokens   + EXCLUDED.tokens,
			    updated_at = now()`,
			today(), d.user, d.req, d.tok)
		if err != nil {
			return err
		}
		p.reported[d.user] = map[string]int{"requests": d.reqAfter, "tokens": d.tokAfter}
	}
	return nil
}

// run flushes deltas and reloads peers' counters. Two timers, because they answer different
// questions: "do my numbers exist elsewhere" and "do I know about theirs".
func (p *pgUsage) run(s *Server, every time.Duration) {
	flush := time.NewTicker(every)
	reload := time.NewTicker(5 * time.Second)
	defer flush.Stop()
	defer reload.Stop()
	for {
		select {
		case <-p.dirty:
			if err := p.save(s); err != nil {
				s.audit(map[string]any{"event": "usage_save_failed", "error": err.Error(),
					"at": time.Now().UTC().Format(time.RFC3339)})
			}
		case <-flush.C:
			if err := p.save(s); err != nil {
				s.audit(map[string]any{"event": "usage_save_failed", "error": err.Error(),
					"at": time.Now().UTC().Format(time.RFC3339)})
			}
		case <-reload.C:
			if _, err := p.load(s); err != nil {
				s.audit(map[string]any{"event": "usage_load_failed", "error": err.Error(),
					"at": time.Now().UTC().Format(time.RFC3339)})
			}
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
