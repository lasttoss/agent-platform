package main

import (
	"sync"
	"time"
)

// A daily cap is a cliff: everyone is fine until the moment nobody is. A token bucket is the
// slope in front of it — per person, refilling continuously, with a small burst so a normal
// working rhythm is not punished.
//
// It is deliberately per user and per process: sharing buckets across replicas needs a shared
// store, and pretending otherwise would be worse than the limit being approximate.
type rateLimiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
	seen   time.Time
}

func newRateLimiter(perMinute, burst int) *rateLimiter {
	if perMinute <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = perMinute
	}
	return &rateLimiter{
		rate:    float64(perMinute) / 60.0,
		burst:   float64(burst),
		buckets: map[string]*bucket{},
		now:     time.Now,
	}
}

// allow takes one token or reports how long the caller should wait. The wait is a real number:
// "try again in 2s" is actionable, "too many requests" is not.
func (l *rateLimiter) allow(userID string) (bool, int) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[userID]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[userID] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	b.seen = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Round up: telling somebody to retry in 0 seconds is a promise the bucket will not keep.
	wait := (1 - b.tokens) / l.rate
	secs := int(wait)
	if float64(secs) < wait {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	return false, secs
}

// sweep drops buckets that have been idle long enough to be full again; otherwise the map grows
// with every person who ever called, which is a slow memory leak with a friendly name.
func (l *rateLimiter) sweep(idle time.Duration) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	removed := 0
	for k, b := range l.buckets {
		if now.Sub(b.seen) > idle {
			delete(l.buckets, k)
			removed++
		}
	}
	return removed
}

func (l *rateLimiter) idle() time.Duration {
	if l == nil || l.rate <= 0 {
		return time.Minute
	}
	// Enough for a full bucket plus slack: a bucket that could have refilled is indistinguishable
	// from one that does not exist yet.
	return time.Duration(l.burst/l.rate*float64(time.Second)) + time.Minute
}
