package main

// A tool that takes minutes cannot be answered by a request that waits: the caller times out, the
// answer is thrown away, and the work is repeated. The gateway turns such a tool into a job — the
// call returns an id immediately and the answer is fetched when it exists.
//
// What this deliberately is not: a queue. There is no worker pool, no retry, no durable job log.
// A job lives in the memory of one process and dies with it. That is enough to stop one HTTP
// request from timing out, and the docs say so instead of implying a scheduler.

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

const (
	jobRunning = "running"
	jobDone    = "done"
	jobFailed  = "failed"
)

type Job struct {
	ID       string
	UserID   string
	Tool     string
	State    string
	Started  time.Time
	Finished time.Time
	Result   map[string]any
	Err      string
}

// view is what a caller may see. The user id is left out: the caller already knows who they are,
// and a job id is not a place to leak somebody else's identity.
func (j Job) view() map[string]any {
	out := map[string]any{"job_id": j.ID, "tool": j.Tool, "state": j.State,
		"started_at": j.Started.UTC().Format(time.RFC3339Nano)}
	if !j.Finished.IsZero() {
		out["finished_at"] = j.Finished.UTC().Format(time.RFC3339Nano)
		out["ms"] = j.Finished.Sub(j.Started).Milliseconds()
	}
	if j.Result != nil {
		out["result"] = j.Result
	}
	if j.Err != "" {
		out["error"] = j.Err
	}
	return out
}

type JobStore struct {
	mu    sync.Mutex
	jobs  map[string]*Job
	ttl   time.Duration
	limit int // running jobs per user: a burst of jobs is a burst of goroutines
	// Per user, so one caller cannot spend the whole process.
	running map[string]int

	started int
	done    int
	failed  int
	refused int
}

func NewJobStore(ttl time.Duration) *JobStore {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &JobStore{jobs: map[string]*Job{}, ttl: ttl, limit: 4, running: map[string]int{}}
}

func newJobID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// create reserves a slot for a user. It refuses rather than queues: a caller told "try later" can
// decide, a caller silently queued behind four builds cannot.
func (s *JobStore) create(userID, tool string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[userID] >= s.limit {
		s.refused++
		return nil, false
	}
	id := newJobID()
	if id == "" {
		s.refused++
		return nil, false
	}
	j := &Job{ID: id, UserID: userID, Tool: tool, State: jobRunning, Started: time.Now()}
	s.jobs[id] = j
	s.running[userID]++
	s.started++
	return j, true
}

func (s *JobStore) finish(id string, result map[string]any, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return
	}
	j.Finished = time.Now()
	if err != nil {
		j.State = jobFailed
		j.Err = err.Error()
		s.failed++
	} else {
		j.State = jobDone
		j.Result = result
		s.done++
	}
	if s.running[j.UserID] > 0 {
		s.running[j.UserID]--
	}
}

// get is scoped to the owner: a job id belonging to somebody else is indistinguishable from an
// id that does not exist, which is the answer that leaks least.
func (s *JobStore) get(id, userID string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.UserID != userID {
		return Job{}, false
	}
	return *j, true
}

func (s *JobStore) stats() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	byState := map[string]int{jobRunning: 0, jobDone: 0, jobFailed: 0}
	for _, j := range s.jobs {
		byState[j.State]++
	}
	return map[string]any{"tracked": len(s.jobs), "running": byState[jobRunning],
		"done": s.done, "failed": s.failed, "started": s.started, "refused": s.refused}
}

// sweep drops finished jobs older than the TTL. Exported as a method so a test can call it
// instead of waiting five minutes.
func (s *JobStore) sweep(now time.Time) {
	cutoff := now.Add(-s.ttl)
	s.mu.Lock()
	for id, j := range s.jobs {
		if !j.Finished.IsZero() && j.Finished.Before(cutoff) {
			delete(s.jobs, id)
		}
	}
	s.mu.Unlock()
}

// run drops finished jobs after the TTL. An unbounded map of answers is a memory leak with a
// nicer name.
func (s *JobStore) run(every time.Duration) {
	if every <= 0 {
		every = 5 * time.Minute
	}
	for range time.Tick(every) {
		s.sweep(time.Now())
	}
}
