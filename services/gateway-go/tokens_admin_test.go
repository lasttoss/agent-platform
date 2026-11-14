package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func tokenJSON(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Tokens map[string]string `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("token file is not valid JSON after a write: %v (%s)", err, raw)
	}
	return doc.Tokens
}

func doJSON(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rdr)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func TestMeTellsAPersonWhatTheyMayReach(t *testing.T) {
	f := newOA(t)
	status, body := doJSON(t, http.MethodGet, f.base+"/me", f.dev, "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var doc struct {
		User        string   `json:"user"`
		Role        string   `json:"role"`
		Models      []string `json:"models"`
		Admin       bool     `json:"admin"`
		TokenMasked string   `json:"token_masked"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.User != "2002" || doc.Role != "dev" || doc.Admin {
		t.Fatalf("/me: %s", body)
	}
	if strings.Join(doc.Models, ",") != "gpt-4.1-mini" {
		t.Fatalf("models: %v", doc.Models)
	}
	if doc.TokenMasked == "" || strings.Contains(body, f.dev) {
		t.Fatalf("the answer must show a mask, never the token: %s", body)
	}
}

func TestRotateReplacesTheTokenAndTheOldOneStops(t *testing.T) {
	f := newOA(t)
	status, body := doJSON(t, http.MethodPost, f.base+"/me/token/rotate", f.dev, "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var doc struct{ Token string }
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Token == "" || doc.Token == f.dev {
		t.Fatalf("rotate did not issue a new token: %s", body)
	}
	// The new token works and keeps the same user.
	if status, _ := doJSON(t, http.MethodGet, f.base+"/me", doc.Token, ""); status != http.StatusOK {
		t.Fatalf("new token refused: %d", status)
	}
	// The old one is gone on the next request, not on the next restart.
	if status, _ := doJSON(t, http.MethodGet, f.base+"/me", f.dev, ""); status != http.StatusUnauthorized {
		t.Fatalf("old token still works: %d", status)
	}
	// The file on disk is the source of truth and is still valid JSON.
	tokens := tokenJSON(t, f.tokensPath)
	if _, ok := tokens[doc.Token]; !ok {
		t.Fatalf("rotated token missing from the file: %v", tokens)
	}
	if _, ok := tokens[f.dev]; ok {
		t.Fatalf("old token left in the file: %v", tokens)
	}
}

func TestAdminSeesMasksAndCannotLeakTheToken(t *testing.T) {
	f := newOA(t)
	status, body := doJSON(t, http.MethodGet, f.base+"/admin/tokens", f.owner, "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if strings.Contains(body, "tok-owner") || strings.Contains(body, "tok-dev") {
		t.Fatalf("the list reproduced a token: %s", body)
	}
	var doc struct {
		Tokens []struct{ ID, User, Masked string }
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Tokens) != 2 {
		t.Fatalf("rows: %s", body)
	}
	if doc.Tokens[0].ID == "" || doc.Tokens[0].Masked == "" {
		t.Fatalf("row without id or mask: %s", body)
	}
	// A dev has no business here at all.
	if status, _ := doJSON(t, http.MethodGet, f.base+"/admin/tokens", f.dev, ""); status != http.StatusForbidden {
		t.Fatalf("dev read the token list: %d", status)
	}
}

func TestIssueAndRevokeATokenForSomebodyElse(t *testing.T) {
	f := newOA(t)
	status, body := doJSON(t, http.MethodPost, f.base+"/admin/tokens", f.owner, `{"user":"2002"}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var made struct{ Token, ID string }
	if err := json.Unmarshal([]byte(body), &made); err != nil {
		t.Fatal(err)
	}
	if status, _ := doJSON(t, http.MethodGet, f.base+"/me", made.Token, ""); status != http.StatusOK {
		t.Fatalf("issued token refused: %d", status)
	}
	if _, ok := tokenJSON(t, f.tokensPath)[made.Token]; !ok {
		t.Fatalf("issued token not written to the file")
	}
	if status, body := doJSON(t, http.MethodDelete, f.base+"/admin/tokens/"+made.ID, f.owner, ""); status != http.StatusOK {
		t.Fatalf("revoke: %d %s", status, body)
	}
	if status, _ := doJSON(t, http.MethodGet, f.base+"/me", made.Token, ""); status != http.StatusUnauthorized {
		t.Fatalf("revoked token still works: %d", status)
	}
	// An id nobody holds is a 404, not a 500 and not silence.
	if status, _ := doJSON(t, http.MethodDelete, f.base+"/admin/tokens/deadbeef", f.owner, ""); status != http.StatusNotFound {
		t.Fatalf("unknown id: %d", status)
	}
}

func TestATokenForAnUnknownUserIsRefused(t *testing.T) {
	f := newOA(t)
	status, body := doJSON(t, http.MethodPost, f.base+"/admin/tokens", f.owner, `{"user":"9999"}`)
	if status != http.StatusNotFound || !strings.Contains(body, "unknown user") {
		t.Fatalf("status=%d body=%s", status, body)
	}
}

func TestRevokingYourOwnTokenIsRefusedWithTheRightAdvice(t *testing.T) {
	f := newOA(t)
	_, list := doJSON(t, http.MethodGet, f.base+"/admin/tokens", f.owner, "")
	var doc struct {
		Tokens []struct{ ID, User string }
	}
	json.Unmarshal([]byte(list), &doc)
	mine := ""
	for _, row := range doc.Tokens {
		if row.User == "1001" {
			mine = row.ID
		}
	}
	status, body := doJSON(t, http.MethodDelete, f.base+"/admin/tokens/"+mine, f.owner, "")
	if status != http.StatusConflict || !strings.Contains(body, "rotate") {
		t.Fatalf("status=%d body=%s", status, body)
	}
}
