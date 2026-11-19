package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A run id ties the steps of one piece of work together.
//
// Without it every audit line is alone, and the only question a support ticket asks -- "what
// happened to this request, in order, and which step was the one that failed" -- cannot be
// answered from the log. The trace already answers it for whoever holds the trace id, but a
// trace is sampled, exported to a third party, and gone in a week. The audit file is the
// durable record, so the durable record needs the join key.
//
// Three rules keep this from becoming a data leak:
//
//   - It is an opaque 128-bit id. No user id, no tool name, no timestamp inside it.
//   - The caller may supply one (X-Run-Id) so separate HTTP calls can be stitched together; an
//     unusable value is replaced rather than reflected back. A caller that sends garbage gets a
//     fresh id, never its own bytes echoed into a log line.
//   - It is not an authorisation. A run id says these steps belong together; it never says who
//     may run them. Two different rules, and mixing them is how a join key becomes a bypass.

const (
	runIDHeader = "X-Run-Id"
	// runIDMax keeps a caller from writing a novel into every line it touches.
	runIDMax = 64
)

type runIDKey struct{}

// runIDFrom returns the run id for this request: the caller's when it is well formed, otherwise
// a new one. The id comes back too, because a handler puts it on the response so the caller can
// quote it in a ticket instead of describing the request from memory.
func runIDFrom(r *http.Request) (context.Context, string) {
	if v := strings.TrimSpace(r.Header.Get(runIDHeader)); usableRunID(v) {
		return context.WithValue(r.Context(), runIDKey{}, v), v
	}
	id := newRunID()
	return context.WithValue(r.Context(), runIDKey{}, id), id
}

// runIDInto is runIDFrom for a handler that already extracted its trace context and no longer
// has the request's own context: it reuses that context so the span and the run agree.
func runIDInto(ctx context.Context, r *http.Request) (context.Context, string) {
	if v := strings.TrimSpace(r.Header.Get(runIDHeader)); usableRunID(v) {
		return context.WithValue(ctx, runIDKey{}, v), v
	}
	id := newRunID()
	return context.WithValue(ctx, runIDKey{}, id), id
}

// RunID is the id in the context, or "" when there is none. Every audit line calls this, so a
// line written outside a request (a startup note, a directory refresh) simply has no run id
// instead of inventing one.
func RunID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(runIDKey{}).(string); ok {
		return v
	}
	return ""
}

// withRunID stamps an audit record. Kept separate from the call sites so no line can be added
// later with the id forgotten.
func withRunID(ctx context.Context, rec map[string]any) map[string]any {
	if id := RunID(ctx); id != "" {
		rec["run_id"] = id
	}
	return rec
}

func newRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// usableRunID accepts only what this gateway is willing to write into a log line and echo back
// in a header: a bounded, printable, no-space token. Anything else is dropped, not sanitised --
// sanitising invents a value the caller did not send, which is worse than a new one.
func usableRunID(v string) bool {
	if v == "" || len(v) > runIDMax {
		return false
	}
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}
