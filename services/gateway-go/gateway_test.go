package main

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	policyPath  = "../gateway/config/policy.json"
	consentPath = "../../config/consent.json"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	pol, err := LoadPolicy(policyPath)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	consent, err := LoadConsent(consentPath)
	if err != nil {
		t.Fatalf("consent: %v", err)
	}
	audit := filepath.Join(t.TempDir(), "audit.log")
	return NewServer(pol, consent, audit)
}

func ok(t *testing.T, s *Server, user, chat, text string) map[string]any {
	t.Helper()
	code, body := s.Handle(respondRequest{PlatformUserID: user, ChatType: chat, Text: text})
	if code != 200 {
		t.Fatalf("expected 200, got %d: %v", code, body)
	}
	return body
}

func TestUnknownUserIsRejected(t *testing.T) {
	s := testServer(t)
	code, body := s.Handle(respondRequest{PlatformUserID: "9999", ChatType: "dm", Text: "lương"})
	if code != 403 {
		t.Fatalf("expected 403, got %d", code)
	}
	if body["denied"].(map[string]any)["code"] != "unknown_user" {
		t.Fatalf("wrong denial: %v", body)
	}
}

func TestDMGetsPersonalAnswer(t *testing.T) {
	s := testServer(t)
	body := ok(t, s, "1001", "dm", "lương tháng này của tôi")
	if got := body["chat_reply"]; got != "Lương tháng này của bạn là 12.345.000 VND." {
		t.Fatalf("unexpected reply: %v", got)
	}
}

func TestPersonalAnswerInGroupGoesToDM(t *testing.T) {
	s := testServer(t)
	body := ok(t, s, "1001", "group_internal", "lương tháng này của tôi")
	if body["delivered"] != "dm" {
		t.Fatalf("expected dm delivery, got %v", body["delivered"])
	}
	if body["chat_reply"] != "Mình gửi riêng cho bạn nhé." {
		t.Fatalf("group must not carry the number: %v", body)
	}
	dm := body["dm"].(map[string]any)
	if dm["text"] != "Lương tháng này của bạn là 12.345.000 VND." {
		t.Fatalf("dm text wrong: %v", dm)
	}
}

func TestPublicGroupHasNoPersonalTool(t *testing.T) {
	s := testServer(t)
	body := ok(t, s, "1001", "group_public", "lương tháng này của tôi")
	visible := body["tools_visible"].([]string)
	if len(visible) != 1 || visible[0] != "docs_search" {
		t.Fatalf("public group must see only docs_search, got %v", visible)
	}
	if body["chat_reply"] == "Lương tháng này của bạn là 12.345.000 VND." {
		t.Fatal("salary leaked into a public group")
	}
}

func TestNoDiacriticsIsUnderstood(t *testing.T) {
	s := testServer(t)
	body := ok(t, s, "1001", "dm", "luong thang nay cua toi")
	if body["chat_reply"] != "Lương tháng này của bạn là 12.345.000 VND." {
		t.Fatalf("folded input not understood: %v", body["chat_reply"])
	}
	body = ok(t, s, "1001", "dm", "tai lieu runbook")
	if body["chat_reply"] != "Tài liệu liên quan: runbook-restart" {
		t.Fatalf("folded docs query wrong: %v", body["chat_reply"])
	}
}

func TestDestructiveToolIsNeverOffered(t *testing.T) {
	s := testServer(t)
	if _, ok := Tools["deploy_prod"]; !ok {
		t.Fatal("deploy_prod should exist in the registry, only be unreachable")
	}
	for _, chat := range []string{"dm", "group_internal", "group_public"} {
		pal, err := s.Policy.Resolve("1001", chat)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range append(s.Policy.VisibleTools(pal), s.Policy.RoutableTools(pal)...) {
			if name == "deploy_prod" {
				t.Fatalf("deploy_prod reachable in %s", chat)
			}
		}
	}
}

func TestConsentGatesProviderTools(t *testing.T) {
	s := testServer(t)
	granted := ok(t, s, "1001", "dm", "trạng thái")["tools_visible"].([]string)
	if !has(granted, "drive_search") || !has(granted, "mail_list") {
		t.Fatalf("consented user should see provider tools: %v", granted)
	}
	not := ok(t, s, "2002", "dm", "trạng thái")["tools_visible"].([]string)
	if has(not, "drive_search") || has(not, "mail_list") {
		t.Fatalf("unconsented user must not see provider tools: %v", not)
	}
	if !has(not, "docs_search") {
		t.Fatalf("a tool with no provider needs no consent: %v", not)
	}
}

func TestSecondIdenticalQuestionComesFromCache(t *testing.T) {
	s := testServer(t)
	ok(t, s, "1001", "dm", "tài liệu runbook")
	body := ok(t, s, "1001", "dm", "tài liệu runbook")
	cache, okc := body["cache"].(map[string]any)
	if !okc || cache["layer"] != "exact" {
		t.Fatalf("expected an exact cache hit, got %v", body["cache"])
	}
}

func TestSalaryIsNeverCached(t *testing.T) {
	s := testServer(t)
	ok(t, s, "1001", "dm", "lương tháng này của tôi")
	body := ok(t, s, "1001", "dm", "lương tháng này của tôi")
	if _, cached := body["cache"]; cached {
		t.Fatalf("a salary answer was cached: %v", body)
	}
}

func TestCacheDoesNotLeakBetweenUsers(t *testing.T) {
	s := testServer(t)
	ok(t, s, "1001", "dm", "lương tháng này của tôi")
	body := ok(t, s, "2002", "dm", "lương tháng này của tôi")
	if body["chat_reply"] != "Lương tháng này của bạn là 9.876.000 VND." {
		t.Fatalf("another user's salary leaked: %v", body["chat_reply"])
	}
}

func TestQuotaIsEnforced(t *testing.T) {
	s := testServer(t)
	s.Policy.Quota.RequestsPerDay = 1
	ok(t, s, "1001", "dm", "trạng thái")
	code, body := s.Handle(respondRequest{PlatformUserID: "1001", ChatType: "dm", Text: "trạng thái"})
	if code != 429 {
		t.Fatalf("expected 429, got %d: %v", code, body)
	}
}

func TestNoToolQuestionIsRefusedWithoutAModel(t *testing.T) {
	s := testServer(t)
	body := ok(t, s, "1001", "dm", "thời tiết hôm nay thế nào")
	if body["chat_reply"] != "Mình chưa có công cụ cho việc này." {
		t.Fatalf("ungrounded answer without a model: %v", body["chat_reply"])
	}
}

func TestFoldAndKeys(t *testing.T) {
	if Fold("LƯƠNG tháng này") != Fold("luong thang nay") {
		t.Fatalf("fold mismatch: %q vs %q", Fold("LƯƠNG tháng này"), Fold("luong thang nay"))
	}
	a := ExactKey("lương", []string{"a", "b"}, "v1", "dm", "user:1")
	b := ExactKey("luong", []string{"b", "a"}, "v1", "dm", "user:1")
	if a != b {
		t.Fatal("tool order or diacritics changed the cache key")
	}
	c := ExactKey("lương", []string{"a", "b"}, "v1", "dm", "user:2")
	if a == c {
		t.Fatal("scope must be part of the cache key")
	}
	if PrefixFingerprint([]string{"z", "a"}, "v1") != PrefixFingerprint([]string{"a", "z"}, "v1") {
		t.Fatal("prefix fingerprint must not depend on order")
	}
	if PrefixFingerprint([]string{"a"}, "v1") == PrefixFingerprint([]string{"a"}, "v2") {
		t.Fatal("prefix fingerprint must change with the policy version")
	}
}

func TestLocalOverrideKeepsRealValuesOutOfGit(t *testing.T) {
	pol, err := LoadPolicy(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	consent, _ := LoadConsent(consentPath)
	localPath := filepath.Join(t.TempDir(), "local.json")
	os.WriteFile(localPath, []byte(`{"users":{"424242":{"name":"Local","role":"owner","workspace":"x"}},
		"salary":{"424242":{"amount":7654321,"currency":"VND"}}}`), 0o644)
	local, err := LoadLocal(localPath)
	if err != nil {
		t.Fatal(err)
	}
	ApplyLocal(pol, consent, local)
	pal, err := pol.Resolve("424242", "dm")
	if err != nil {
		t.Fatalf("local user should resolve: %v", err)
	}
	res, _ := CallTool("salary_self", nil, pal)
	if res["amount"] != 7654321 {
		t.Fatalf("local salary override ignored: %v", res)
	}
}

func has(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
