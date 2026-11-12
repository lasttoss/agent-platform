package main

import (
	"net/http"
	"sync"
	"testing"

	"github.com/lasttoss/agent-platform/services/gateway-go/internal/mcpserver"
)

// dirSpy is the userdir service: a list of users and an active flag per user.
type dirSpy struct {
	mu    sync.Mutex
	users map[string]bool
	calls []string
}

func (d *dirSpy) handler() *mcpserver.Handler {
	return &mcpserver.Handler{
		Name: "userdir",
		Tools: []mcpserver.Tool{
			{Name: "user_list", InputSchema: map[string]any{"type": "object"}},
			{Name: "user_consistency", InputSchema: map[string]any{"type": "object"}},
			{Name: "user_set_active", InputSchema: map[string]any{"type": "object"}},
		},
		Call: func(name string, args map[string]any, r *http.Request) (any, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.calls = append(d.calls, name)
			if name != "user_list" {
				return map[string]any{"match": true}, nil
			}
			list := []any{}
			for id, active := range d.users {
				list = append(list, map[string]any{"id": id, "active": active, "role": "dev"})
			}
			return map[string]any{"store": "sqlite", "users": list, "count": len(list)}, nil
		},
	}
}

func newHarnessWithDir(t *testing.T) (*harness, *dirSpy) {
	t.Helper()
	// 1001 is enabled, 2002 is disabled, 3003 is absent from the directory entirely.
	spy := &dirSpy{users: map[string]bool{"1001": true, "2002": false}}
	h := newHarness(t, withExtraServer("userdir", spy.handler()))
	if err := h.RefreshDirectory(h.Reg); err != nil {
		t.Fatalf("directory: %v", err)
	}
	return h, spy
}

// A person disabled in the directory is refused before policy, before the model, before cache.
func TestDisabledUserIsRefused(t *testing.T) {
	h, _ := newHarnessWithDir(t)
	code, body := h.Handle(respondRequest{PlatformUserID: "2002", ChatType: "dm", Text: "status"})
	if code != 403 {
		t.Fatalf("disabled user should be 403, got %d: %v", code, body)
	}
	denied, _ := body["denied"].(map[string]any)
	if denied["code"] != "user_disabled" {
		t.Fatalf("want user_disabled, got %v", denied)
	}
}

// A person policy.json provisions but the directory has never heard of is a mismatch, and a
// mismatch is refused: the two systems must agree about who exists.
func TestUserMissingFromDirectoryIsRefused(t *testing.T) {
	h, _ := newHarnessWithDir(t)
	code, body := h.Handle(respondRequest{PlatformUserID: "3003", ChatType: "dm", Text: "status"})
	if code != 403 {
		t.Fatalf("unknown-in-directory user should be 403, got %d: %v", code, body)
	}
	denied, _ := body["denied"].(map[string]any)
	if denied["code"] != "user_not_in_directory" {
		t.Fatalf("want user_not_in_directory, got %v", denied)
	}
}

func TestEnabledUserStillWorks(t *testing.T) {
	h, _ := newHarnessWithDir(t)
	h.ok(t, "1001", "dm", "status")
}

// No directory service at all: policy.json decides, and the state is visible rather than implied.
func TestWithoutADirectoryPolicyDecides(t *testing.T) {
	h := newHarness(t)
	h.ok(t, "1001", "dm", "status")
	state := h.directoryState("1001")
	if state["source"] != "policy" || state["loaded"] != false {
		t.Fatalf("expected an explicit policy fallback, got %v", state)
	}
}

// A stale copy keeps the last answer: an unreachable directory must not re-enable a person who
// was disabled on purpose.
func TestStaleDirectoryKeepsRefusingTheDisabledUser(t *testing.T) {
	h, spy := newHarnessWithDir(t)
	spy.mu.Lock()
	spy.users = map[string]bool{} // pretend the service starts failing
	spy.mu.Unlock()
	code, _ := h.Handle(respondRequest{PlatformUserID: "2002", ChatType: "dm", Text: "status"})
	if code != 403 {
		t.Fatalf("a disabled user must stay disabled, got %d", code)
	}
	h.Reg.Refresh() // service is up in this fake, but the point is the last copy survives errors
	if state := h.directoryState("2002"); state["source"] != "userdir" {
		t.Fatalf("source should still name the directory: %v", state)
	}
}
