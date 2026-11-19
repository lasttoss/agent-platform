package main

import (
	"context"
	"strconv"
	"time"
)

// Withdrawing access has to be visible on the next request, not whenever a cache entry expires.
//
// A role change already bumps the role store's generation, which is why the MCP cache key
// carries it. That leaves three ways to lose access that changed nothing:
//
//   - a token is revoked, and the answers it paid for stay in the cache
//   - a token is rotated, and the same person keeps reading entries cached under the old one
//   - a person is disabled in the directory, and the cache has no opinion about that
//
// So one counter covers all three, and it goes into every cache key beside the role generation.
// Two numbers rather than one because they answer different questions: "what is this person
// allowed to do now" is the role store's, "has this person's access been taken away" is this
// one. Merging them would make a debug session read the wrong answer.
//
// It is a counter, not a clock: two revocations in the same second must not look identical.

// bumpRevocation records that access was withdrawn, and writes one audit line saying which.
func (s *Server) bumpRevocation(ctx context.Context, reason string) int64 {
	n := s.revocations.Add(1)
	s.auditRun(ctx, map[string]any{
		"event": "access_withdrawn", "reason": reason, "revocations": n,
		"at": time.Now().UTC().Format(time.RFC3339),
	})
	return n
}

// revocationGeneration is what a cache key carries. A bump makes every entry cached under the
// previous value unreachable without walking the cache, which is the same trick the role
// generation uses and for the same reason: a scan under load is a pause, and a pause is a denial.
func (s *Server) revocationGeneration() int64 { return s.revocations.Load() }

// policyStamp is the complete answer to "under which permission state was this answer produced".
// Both doors build their cache keys from it, so the adapter path cannot quietly keep serving
// what the MCP path has already retired.
func (s *Server) policyStamp() string {
	return PolicyVersion + ":r" + itoa(int(s.policyGeneration())) + ":x" + i64toa(s.revocationGeneration())
}

func itoa(n int) string     { return strconv.Itoa(n) }
func i64toa(n int64) string { return strconv.FormatInt(n, 10) }
