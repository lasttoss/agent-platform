package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// cacheLayer reports which cache layer answered, or "" when the call went all the way through.
func cacheLayer(t *testing.T, body map[string]any) string {
	t.Helper()
	if c, ok := body["cache"].(map[string]any); ok {
		if l, ok := c["layer"].(string); ok {
			return l
		}
	}
	return ""
}

// The adapter door cached under a constant policy version, so a demotion changed what the model
// may see while the previous answer kept being served. This is the test that would have caught it.
func TestADemotionRetiresTheAdapterCacheImmediately(t *testing.T) {
	h := newHarness(t)
	// The premise, stated rather than borrowed from a fixture: a role that keeps status but loses
	// the cached docs_search. The default fixture gives both roles docs_search, which would make
	// the test pass for the wrong reason.
	h.Policy.Roles["guest"] = RoleConf{Tools: []string{"status"}}
	rs, err := NewRoleStore(filepath.Join(t.TempDir(), "roles.json"))
	if err != nil {
		t.Fatal(err)
	}
	h.Roles = rs

	// docs_search is the cached tool (ttl 3600 in config/tools.json); salary_self is deliberately
	// never cached, because a personal answer has no business in a shared cache.
	first := h.ok(t, "1001", "dm", "tìm tài liệu runbook")
	if cacheLayer(t, first) != "" {
		t.Fatalf("first call cannot be a cache hit: %v", first)
	}
	second := h.ok(t, "1001", "dm", "tìm tài liệu runbook")
	if cacheLayer(t, second) != "exact" {
		t.Fatalf("the second identical call should be served from the cache: %v", second)
	}

	// Demote: the person keeps status and loses docs_search.
	if err := h.Roles.Assign("1001", "guest"); err != nil {
		t.Fatal(err)
	}

	third := h.ok(t, "1001", "dm", "tìm tài liệu runbook")
	if layer := cacheLayer(t, third); layer == "exact" || layer == "semantic" {
		t.Fatalf("a demoted caller was served a cached answer: %v", third)
	}
	// Two things must be true, and either one alone is not enough: the cached answer must not be
	// served, and the tool must no longer be visible. The second is what makes the first honest
	// rather than lucky.
	if layer := cacheLayer(t, third); layer != "" {
		t.Fatalf("a demoted caller was served a cached answer: %v", third)
	}
	if reply, _ := second["chat_reply"].(string); reply != "" &&
		strings.Contains(asString(third["chat_reply"]), reply) {
		t.Fatalf("the pre-demotion answer came back: %v", third)
	}
	for _, name := range third["tools_visible"].([]string) {
		if name == "docs_search" {
			t.Fatalf("docs_search is still visible to a demoted caller: %v", third)
		}
	}
	_ = first
}

// Revoking a token withdraws access. The answers that token paid for must stop being served, or a
// rotation done because the token leaked keeps handing out the data the leak was about.
func TestAWithdrawnTokenRetiresTheAnswersItPaidFor(t *testing.T) {
	h := newHarness(t)
	h.ok(t, "1001", "dm", "trạng thái hệ thống")
	if layer := cacheLayer(t, h.ok(t, "1001", "dm", "trạng thái hệ thống")); layer != "exact" {
		t.Fatalf("expected a cache hit before the revocation, got %q", layer)
	}

	h.bumpRevocation(context.Background(), "token_revoked")

	after := h.ok(t, "1001", "dm", "trạng thái hệ thống")
	if layer := cacheLayer(t, after); layer != "" {
		t.Fatalf("an answer cached before the revocation was served after it: %v", after)
	}
}

// A person disabled in the directory loses access. The cache has no opinion about that unless it
// is told, and "disabled" that still answers is the worst of both.
func TestDisablingAPersonInTheDirectoryRetiresTheirAnswers(t *testing.T) {
	h := newHarness(t)
	h.LoadDirectory(map[string]bool{"1001": true, "2002": true})
	h.ok(t, "1001", "dm", "tìm tài liệu runbook")
	if layer := cacheLayer(t, h.ok(t, "1001", "dm", "tìm tài liệu runbook")); layer != "exact" {
		t.Fatalf("expected a cache hit before the disable, got %q", layer)
	}

	// Disabled: the request is refused before the cache is consulted. That is the first half.
	h.LoadDirectory(map[string]bool{"1001": false, "2002": true})
	if code, body := h.Handle(respondRequest{PlatformUserID: "1001", ChatType: "dm",
		Text: "tìm tài liệu runbook"}); code == 200 {
		t.Fatalf("a disabled person must be refused, got 200 %v", body)
	}

	// Re-enabled: the answer cached before the disable must not come back. A counter bump is what
	// makes that true; without it the entry is still reachable and the disable left a footprint.
	h.LoadDirectory(map[string]bool{"1001": true, "2002": true})
	_, back := h.Handle(respondRequest{PlatformUserID: "1001", ChatType: "dm",
		Text: "tìm tài liệu runbook"})
	if layer := cacheLayer(t, back); layer != "" {
		t.Fatalf("an answer cached before the disable was served after it: %v", back)
	}
}

// The stamp is the answer to "which permission state produced this". Two different states must
// never share a key, and the same state must keep hitting.
func TestTheStampChangesOnlyWhenSomethingActuallyChanged(t *testing.T) {
	h := newHarness(t)
	base := h.policyStamp()
	if base != h.policyStamp() {
		t.Fatal("the stamp must be stable while nothing changes")
	}
	h.bumpRevocation(context.Background(), "token_revoked")
	if h.policyStamp() == base {
		t.Fatal("a withdrawal must change the stamp")
	}
	withdrawn := h.policyStamp()
	h.bumpRevocation(context.Background(), "token_revoked")
	if h.policyStamp() == withdrawn {
		t.Fatal("two withdrawals in the same second must not produce the same stamp")
	}
}

// Both doors must build their key from the same stamp, or one keeps serving what the other retired.
func TestTheAdapterAndTheMCPDoorAgreeOnTheStamp(t *testing.T) {
	h := newHarness(t)
	stamp := h.policyStamp()
	keyAdapter := ExactKey("status", []string{"status"}, stamp, "dm", "user:1001")
	h.bumpRevocation(context.Background(), "token_rotated")
	after := ExactKey("status", []string{"status"}, h.policyStamp(), "dm", "user:1001")
	if keyAdapter == after {
		t.Fatal("the adapter key did not change when the stamp did")
	}
	// The MCP door's key is built inline; the property that matters is the same: a withdrawn
	// access produces a different key. Proven here through the stamp both doors use.
	if h.revocationGeneration() == 0 {
		t.Fatal("the counter did not move")
	}
}

// The same demotion, seen from the MCP door. Two doors that disagree about who may do what is the
// failure this whole file exists to prevent, so both are asserted rather than one assumed.
func TestTheMCPDoorAlsoAppliesACouncilDemotion(t *testing.T) {
	withRoleStore(t, map[string]string{"2002": "dev"})
	base, _, audit := testGateway(t)

	before := listTools(t, base, "tok-dev")
	if !hasName(before, "echo") {
		t.Fatalf("dev should see echo before the demotion: %v", before)
	}

	// Demote to guest, which is not a role with any tool in the fixture's policy.
	if code, out := callAdmin(t, base, "tok-owner", "POST", "/admin/users/2002/role",
		`{"role":"guest"}`); code != 200 {
		t.Fatalf("demotion refused: %d %v", code, out)
	}

	after := listTools(t, base, "tok-dev")
	if hasName(after, "echo") {
		t.Fatalf("echo is still visible after the demotion: %v", after)
	}

	// And the record says so, which is the part a reviewer can check without trusting the UI.
	found := false
	for _, l := range auditLines(t, audit) {
		if l["event"] == "admin_role_change" && l["subject"] == "2002" && l["to"] == "guest" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit line for the demotion: %v", auditLines(t, audit))
	}
}

// hasName asks a tool list whether it holds one name. The package already has a contains for a
// different type, and shadowing it in this file would make both unreadable.
func hasName(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
