package main

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/lasttoss/agent-platform/services/gateway-go/internal/mcpserver"
)

// ocrSpy is a service that behaves like services/ocr: submit returns an id, read returns the job.
type ocrSpy struct {
	mu    sync.Mutex
	calls []string          // "name:job_id"
	state map[string]string // job id -> status
}

func (o *ocrSpy) handler() *mcpserver.Handler {
	return &mcpserver.Handler{
		Name: "ocr",
		Tools: []mcpserver.Tool{
			{Name: "ocr_extract", InputSchema: map[string]any{"type": "object"}},
			{Name: "ocr_result", InputSchema: map[string]any{"type": "object"}},
		},
		Call: func(name string, args map[string]any, r *http.Request) (any, error) {
			o.mu.Lock()
			defer o.mu.Unlock()
			switch name {
			case "ocr_extract":
				id := "job-1"
				o.state[id] = "done"
				o.calls = append(o.calls, name+":")
				return map[string]any{"job_id": id, "status": "queued"}, nil
			default:
				id := asString(args["job_id"])
				o.calls = append(o.calls, name+":"+id)
				return map[string]any{"status": o.state[id],
					"result": map[string]any{"chars": 420, "pages": 2}}, nil
			}
		},
	}
}

func (o *ocrSpy) seen() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string{}, o.calls...)
}

// The default OCR path is a queue, not a block: the first turn submits and answers immediately,
// the second turn reads the job. The job id is the gateway's state, not a string a person or a
// model has to carry around.
func TestExtractionIsQueuedAndReadBackWithoutTheUserTypingAnId(t *testing.T) {
	spy := &ocrSpy{state: map[string]string{}}
	h := newHarness(t, withExtraServer("ocr", spy.handler()))

	first := h.ok(t, "1001", "dm", "read samples/text.pdf")
	reply := first["chat_reply"].(string)
	if !strings.Contains(reply, "job-1") {
		t.Fatalf("submit should name the job: %q", reply)
	}
	if calls := spy.seen(); len(calls) != 1 || calls[0] != "ocr_extract:" {
		t.Fatalf("first turn must only submit: %v", calls)
	}

	second := h.ok(t, "1001", "dm", "result")
	if got := second["chat_reply"].(string); !strings.Contains(got, "420 characters") {
		t.Fatalf("second turn should report the finished job: %q", got)
	}
	calls := spy.seen()
	if len(calls) != 2 || calls[1] != "ocr_result:job-1" {
		t.Fatalf("the gateway must fill the job id itself: %v", calls)
	}
}

// Asking for a result with nothing submitted is a sentence, not a tool call and not an error.
func TestResultWithoutAJobSaysSo(t *testing.T) {
	spy := &ocrSpy{state: map[string]string{}}
	h := newHarness(t, withExtraServer("ocr", spy.handler()))

	body := h.ok(t, "1001", "dm", "result")
	if !strings.Contains(body["chat_reply"].(string), "No extraction job") {
		t.Fatalf("expected a plain answer, got %q", body["chat_reply"])
	}
	if calls := spy.seen(); len(calls) != 0 {
		t.Fatalf("no job means no service call: %v", calls)
	}
}

// One person's pending job must never answer another person's question.
func TestPendingJobIsPerUser(t *testing.T) {
	spy := &ocrSpy{state: map[string]string{}}
	h := newHarness(t, withExtraServer("ocr", spy.handler()))

	h.ok(t, "1001", "dm", "read samples/text.pdf")
	body := h.ok(t, "2002", "dm", "result")
	if !strings.Contains(body["chat_reply"].(string), "No extraction job") {
		t.Fatalf("user 2002 has no job of their own: %q", body["chat_reply"])
	}
}
