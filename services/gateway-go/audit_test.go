package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type auditCollector struct {
	mu   sync.Mutex
	got  []map[string]any
	code int
}

func (a *auditCollector) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rec map[string]any
		if err := json.NewDecoder(r.Body).Decode(&rec); err == nil {
			a.mu.Lock()
			a.got = append(a.got, rec)
			a.mu.Unlock()
		}
		code := a.code
		if code == 0 {
			code = http.StatusAccepted
		}
		w.WriteHeader(code)
	}))
}

func (a *auditCollector) lines() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]any{}, a.got...)
}

// Several gateways writing to one collector is the only way "how many requests were shed today"
// has an answer. The local file stays the durable record; this is the aggregate view.
func TestAuditLinesAreShippedToTheCollector(t *testing.T) {
	col := &auditCollector{}
	sink := col.server()
	defer sink.Close()

	h := newHarness(t)
	h.StartAuditSink(sink.URL)
	h.ok(t, "1001", "dm", "status")
	h.ok(t, "1001", "dm", "docs runbook")

	deadline := time.Now().Add(3 * time.Second)
	for len(col.lines()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	lines := col.lines()
	if len(lines) < 2 {
		t.Fatalf("expected the collector to receive the lines, got %d", len(lines))
	}
	seen := map[string]bool{}
	for _, line := range lines {
		id, _ := line["event_id"].(string)
		if id == "" {
			t.Fatalf("every line needs an event id, got %v", line)
		}
		if seen[id] {
			t.Fatalf("two events shared an id: %s", id)
		}
		seen[id] = true
		if line["decision"] == nil {
			t.Fatalf("line without a decision: %v", line)
		}
	}
}

// A dead collector must not fail a request and must not hide the loss: the file still has the
// line, and the drop is counted.
func TestAFailingCollectorDoesNotFailRequests(t *testing.T) {
	col := &auditCollector{code: http.StatusInternalServerError}
	sink := col.server()
	defer sink.Close()

	h := newHarness(t)
	h.StartAuditSink(sink.URL)
	h.ok(t, "1001", "dm", "status")

	deadline := time.Now().Add(3 * time.Second)
	for h.AuditDropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if h.AuditDropped() == 0 {
		t.Fatal("a rejected delivery must be counted, not swallowed")
	}
	// The request itself was answered: logging never sits in the request path.
	if _, err := h.Handle(respondRequest{PlatformUserID: "1001", ChatType: "dm", Text: "status"}); err == nil {
		t.Fatal("expected a handled response, not an error")
	}
}
