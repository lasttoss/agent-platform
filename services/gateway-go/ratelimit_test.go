package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestBucketRefillsAndRefuses(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRateLimiter(60, 2) // one per second, burst of two
	l.now = func() time.Time { return now }

	if ok, _ := l.allow("a"); !ok {
		t.Fatal("first call must pass")
	}
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("second call must pass inside the burst")
	}
	ok, wait := l.allow("a")
	if ok {
		t.Fatal("third call must be refused")
	}
	if wait != 1 {
		t.Fatalf("want a 1s wait, got %ds", wait)
	}
	// Another user is unaffected: a shared bucket would punish the wrong person.
	if ok, _ := l.allow("b"); !ok {
		t.Fatal("a second user must have their own bucket")
	}
	now = now.Add(time.Second)
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("one second later the bucket must have refilled")
	}
}

func TestBucketSweep(t *testing.T) {
	now := time.Unix(2000, 0)
	l := newRateLimiter(60, 2)
	l.now = func() time.Time { return now }
	l.allow("a")
	l.allow("b")
	now = now.Add(5 * time.Minute)
	if removed := l.sweep(time.Minute); removed != 2 {
		t.Fatalf("want 2 idle buckets removed, got %d", removed)
	}
}

func TestRateLimitOnMCP(t *testing.T) {
	// One request per minute, burst of one: the second call in quick succession is refused and
	// told how long to wait, and the refusal is audited.
	testRate = struct{ perMinute, burst int }{perMinute: 1, burst: 1}
	t.Cleanup(func() { testRate = struct{ perMinute, burst int }{} })
	base, _, audit := testGateway(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"q":"r"}}}`
	if _, resp := call(t, base, "tok-owner", body, ""); !strings.Contains(resp, "result") {
		t.Fatalf("first call should pass: %s", resp)
	}
	_, resp := call(t, base, "tok-owner", body, "")
	if !strings.Contains(resp, "rate limited") {
		t.Fatalf("second call should be rate limited: %s", resp)
	}
	raw, _ := os.ReadFile(audit)
	if !strings.Contains(string(raw), "mcp_rate_limited") {
		t.Fatal("a refusal must be audited")
	}
	if _, other := call(t, base, "tok-dev", body, ""); strings.Contains(other, "rate limited") {
		t.Fatalf("another user must keep their own bucket: %s", other)
	}
}
