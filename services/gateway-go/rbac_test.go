package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func names(vs []ToolVerdict, keep func(ToolVerdict) bool) []string {
	out := []string{}
	for _, v := range vs {
		if keep(v) {
			out = append(out, v.Tool)
		}
	}
	sort.Strings(out)
	return out
}

func blockedBy(vs []ToolVerdict, tool string) string {
	for _, v := range vs {
		if v.Tool == tool {
			return v.BlockedBy
		}
	}
	return "<absent>"
}

// Explain is a second implementation of the same decision, so the only honest test is agreement
// with the one the request path actually uses. If these two ever disagree, one of them is lying
// to a person asking "why can't the model see my tool?".
func TestExplainAgreesWithTheRequestPath(t *testing.T) {
	for _, tc := range []struct{ user, chat string }{
		{"1001", "dm"}, {"1001", "group_internal"}, {"1001", "group_public"},
		{"2002", "dm"}, {"2002", "group_public"}, {"3003", "dm"},
	} {
		t.Run(tc.user+"/"+tc.chat, func(t *testing.T) {
			h := newHarness(t)
			pal, err := h.Policy.Resolve(tc.user, tc.chat)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			vs := h.Policy.Explain(pal, h.Reg.Meta, h.Reg, h.Consent)

			visible := names(vs, func(v ToolVerdict) bool { return v.Visible })
			wantVisible := h.Policy.VisibleTools(pal, h.Reg.Meta, h.Reg, h.Consent)
			sort.Strings(wantVisible)
			if strings.Join(visible, ",") != strings.Join(wantVisible, ",") {
				t.Fatalf("visible: explain=%v request-path=%v", visible, wantVisible)
			}

			routable := names(vs, func(v ToolVerdict) bool { return v.Routable })
			wantRoutable := h.Policy.RoutableTools(pal, h.Reg.Meta, h.Reg, h.Consent)
			sort.Strings(wantRoutable)
			if strings.Join(routable, ",") != strings.Join(wantRoutable, ",") {
				t.Fatalf("routable: explain=%v request-path=%v", routable, wantRoutable)
			}
		})
	}
}

// The question this endpoint exists for: a public group should hide the salary tool for a
// reason a human can read, not as an empty list.
func TestWhoamiNamesTheGateThatBlockedEachTool(t *testing.T) {
	h := newHarness(t)
	vs := h.Policy.Explain(mustResolve(t, h, "1001", "group_public"), h.Reg.Meta, h.Reg, h.Consent)

	if got := blockedBy(vs, "salary_self"); got != GateNotInChat {
		t.Fatalf("salary_self in group_public: want %s, got %s", GateNotInChat, got)
	}
	if got := blockedBy(vs, "ocr_extract"); got != GateNotInChat {
		t.Fatalf("ocr_extract (service down, not in chat): want %s, got %s", GateNotInChat, got)
	}
	if got := blockedBy(vs, "docs_search"); got != "" {
		t.Fatalf("docs_search should be visible here, got %s", got)
	}
	// A tool that has policy facts and a chat slot but no running service must say so, and that
	// is a different answer from "not for you".
	vsDm := h.Policy.Explain(mustResolve(t, h, "1001", "dm"), h.Reg.Meta, h.Reg, h.Consent)
	if got := blockedBy(vsDm, "drive_search"); got != GateNotAdvertising {
		t.Fatalf("drive_search with no service: want %s, got %s", GateNotAdvertising, got)
	}
}

func mustResolve(t *testing.T, h *harness, user, chat string) Principal {
	t.Helper()
	pal, err := h.Policy.Resolve(user, chat)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return pal
}

// The RBAC gate must be reachable and named, not folded into "not in chat".
func TestWhoamiReportsTheRoleGate(t *testing.T) {
	raw, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	doc["roles"].(map[string]any)["intern"] = map[string]any{"tools": []string{"docs_search"}}
	doc["users"].(map[string]any)["4004"] = map[string]any{"name": "Intern", "role": "intern", "workspace": "x"}
	out, _ := json.Marshal(doc)
	path := filepath.Join(t.TempDir(), "policy.json")
	os.WriteFile(path, out, 0o644)

	h := newHarness(t)
	pol, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	pal, err := pol.Resolve("4004", "dm")
	if err != nil {
		t.Fatal(err)
	}
	vs := pol.Explain(pal, h.Reg.Meta, h.Reg, h.Consent)
	if got := blockedBy(vs, "salary_self"); got != GateNotInRole {
		t.Fatalf("want %s for salary_self, got %s", GateNotInRole, got)
	}
	if got := blockedBy(vs, "docs_search"); got != "" {
		t.Fatalf("docs_search should pass the role gate, got %s", got)
	}
}

func TestWhoamiEndpoint(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/whoami?user=1001&chat=dm")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body struct {
		User struct {
			Role       string `json:"role"`
			PersonalOK bool   `json:"personal_data"`
		} `json:"user"`
		Tools  []ToolVerdict  `json:"tools"`
		Counts map[string]int `json:"counts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.User.Role != "owner" || !body.User.PersonalOK {
		t.Fatalf("principal wrong: %+v", body.User)
	}
	if body.Counts["visible"] == 0 {
		t.Fatal("owner in a dm should see tools")
	}

	resp2, err := http.Get(srv.URL + "/v1/whoami?user=9999&chat=dm")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown user should be 403, got %d", resp2.StatusCode)
	}
}

// Without a token this is a single-host setup. With one, an unauthenticated adapter cannot
// choose which user it speaks for — that is the difference between a transport and a claim.
func TestAdapterTokenFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.AdapterToken = "s3cret"
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	if code := postRespond(t, srv.URL, nil); code != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", code)
	}
	if code := postRespond(t, srv.URL, map[string]string{"X-Adapter-Token": "wrong"}); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: want 401, got %d", code)
	}
	if code := postRespond(t, srv.URL, map[string]string{"X-Adapter-Token": "s3cret"}); code != 200 {
		t.Fatalf("right token: want 200, got %d", code)
	}

	h2 := newHarness(t) // no token configured: unchanged behaviour
	srv2 := httptest.NewServer(h2.Routes())
	defer srv2.Close()
	if code := postRespond(t, srv2.URL, nil); code != 200 {
		t.Fatalf("unset token must not invent an auth requirement, got %d", code)
	}
}

func postRespond(t *testing.T, base string, headers map[string]string) int {
	t.Helper()
	body := `{"platform_user_id":"1001","chat_type":"dm","text":"status"}`
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/respond", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
