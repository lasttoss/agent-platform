package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// auditLines reads the durable record a test wrote. Order matters here: the whole point of a run
// id is that the lines of one run come out adjacent and in order.
func auditLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		rec := map[string]any{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("audit line is not json: %v (%q)", err, line)
		}
		out = append(out, rec)
	}
	return out
}

func runIDsIn(lines []map[string]any) map[string]int {
	seen := map[string]int{}
	for _, l := range lines {
		if id, ok := l["run_id"].(string); ok && id != "" {
			seen[id]++
		}
	}
	return seen
}

// A caller that brings a run id keeps it, and it comes back on the response: that is what makes
// "here is my X-Run-Id" a usable sentence in a support ticket.
func TestACallerSuppliedRunIDIsKeptAndEchoed(t *testing.T) {
	base, _, audit := testGateway(t)
	req, _ := http.NewRequest(http.MethodPost, base+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok-owner")
	req.Header.Set(runIDHeader, "run-abc.123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get(runIDHeader); got != "run-abc.123" {
		t.Fatalf("response should echo the run id, got %q", got)
	}
	lines := auditLines(t, audit)
	if len(lines) == 0 {
		t.Fatal("no audit lines")
	}
	for _, l := range lines {
		if l["run_id"] != "run-abc.123" {
			t.Fatalf("every line of this call belongs to the run, got %v", l)
		}
	}
}

// No run id from the caller means one is minted, and it is unique per request: two identical
// calls are two runs. A shared id would merge them into one story that never happened.
// A ping writes no audit line by design, so its minted id is checked on the response only.
func TestTwoCallsWithoutARunIDGetDifferentOnes(t *testing.T) {
	base, _, audit := testGateway(t)
	for i := 0; i < 2; i++ {
		code, _ := call(t, base, "tok-owner", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`, "")
		if code != 200 {
			t.Fatalf("call %d: %d", i, code)
		}
	}
	seen := runIDsIn(auditLines(t, audit))
	if len(seen) != 2 {
		t.Fatalf("two calls are two runs, got %d (%v)", len(seen), seen)
	}
	for id, n := range seen {
		if !usableRunID(id) {
			t.Fatalf("minted run id is not usable: %q", id)
		}
		if n != 1 {
			t.Fatalf("run %q wrote %d lines, want 1 (echo, uncached, no ttl 0)", id, n)
		}
	}

	// The response header carries the same id, so a caller can quote it without reading the log.
	req, _ := http.NewRequest(http.MethodPost, base+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer tok-owner")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	minted := resp.Header.Get(runIDHeader)
	if !usableRunID(minted) {
		t.Fatalf("ping minted an unusable run id: %q", minted)
	}
	if seen[minted] != 0 {
		t.Fatalf("ping reused the run id of a tool call: %q", minted)
	}
}

// A run id is written into a log line and echoed in a header, so a hostile value must never be
// reflected. It is replaced, not sanitised: a cleaned-up version is a value the caller never sent.
// Go's own client refuses a newline in a header, so the realistic hostile value is one that is
// legal on the wire and tempting to interpolate: quotes, a space and a brace.
func TestAHostileRunIDIsReplacedNotReflected(t *testing.T) {
	base, _, audit := testGateway(t)
	hostile := `abc {"event":"forged"}`
	req, _ := http.NewRequest(http.MethodPost, base+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok-owner")
	req.Header.Set(runIDHeader, hostile)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := resp.Header.Get(runIDHeader)
	if got == hostile || strings.Contains(got, "forged") || strings.Contains(got, " ") {
		t.Fatalf("hostile run id was reflected: %q", got)
	}
	if !usableRunID(got) {
		t.Fatalf("replacement is not usable: %q", got)
	}
	lines := auditLines(t, audit)
	if len(lines) == 0 {
		t.Fatal("the tool call wrote no audit line")
	}
	for _, l := range lines {
		if id, _ := l["run_id"].(string); id != got {
			t.Fatalf("line should carry the replacement id %q, got %v", got, l)
		}
		if _, forged := l["event"].(string); forged && l["event"] == `forged` {
			t.Fatalf("a run id invented an event: %v", l)
		}
	}
}

// The adapter path (/v1/respond) is the second door; a run id that only covers /mcp would leave
// half the record unjoinable.
func TestTheAdapterDoorAlsoCarriesARunID(t *testing.T) {
	h := newHarness(t)
	if _, body := h.HandleCtx(context.Background(), respondRequest{
		PlatformUserID: "1001", ChatType: "dm", Text: "trạng thái"}); body == nil {
		t.Fatal("no body")
	}
	lines := auditLines(t, h.Audit)
	if len(lines) == 0 {
		t.Fatal("no audit lines")
	}
	// HandleCtx with a bare context has no run: the line must still be written and simply carry
	// no run id, rather than inventing one that joins to nothing.
	for _, l := range lines {
		if _, has := l["run_id"]; has {
			t.Fatalf("a line outside a run must not claim one: %v", l)
		}
	}
	ctx, run := runIDInto(context.Background(), mustRequest(t, runIDHeader, "adapter-1"))
	if _, body := h.HandleCtx(ctx, respondRequest{
		PlatformUserID: "1001", ChatType: "dm", Text: "trạng thái"}); body == nil {
		t.Fatal("no body")
	}
	found := false
	for _, l := range auditLines(t, h.Audit) {
		if l["run_id"] == run {
			found = true
		}
	}
	if !found {
		t.Fatalf("no line carried the adapter run id %q", run)
	}
}

// mustRequest builds a request carrying one header, for tests that need a context without an
// HTTP server.
func mustRequest(t *testing.T, key, value string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "http://example.invalid/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(key, value)
	return r
}
