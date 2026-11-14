package main

// Token self-service: a person gets their own credential from the console, and an admin can hand
// one out or take it away. The file stays the source of truth — these handlers write it, and the
// hot-reload path already in tokens.go puts the change into force on the next request.
//
// Two rules decide the shape:
//
//   - a token is shown exactly once, at creation. A store that can re-read a secret is a store
//     that leaks it; the list shows a mask, and "I lost it" means "rotate", not "show me".
//   - nobody can revoke their own way out. Rotating your own token is allowed and returns the
//     replacement in the same answer; revoking your own last admin token is refused.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type tokenRow struct {
	ID     string `json:"id"`     // stable, derived: short sha256 of the token
	User   string `json:"user"`   // policy user id
	Masked string `json:"masked"` // "dsh-…oken"
}

// tokenID is the handle a console uses. Hashing the token means the list can be stable and
// shareable without the token itself ever leaving the server.
func tokenID(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:4])
}

func (s *Server) tokenRows() []tokenRow {
	rows := []tokenRow{}
	if s.MCP == nil {
		return rows
	}
	for tok, user := range s.MCP.tokens.snapshot() {
		rows = append(rows, tokenRow{ID: tokenID(tok), User: user, Masked: maskToken(tok)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].User != rows[j].User {
			return rows[i].User < rows[j].User
		}
		return rows[i].ID < rows[j].ID
	})
	return rows
}

func newToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "tok_" + hex.EncodeToString(b), nil
}

// me answers the console's "who am I and what may I reach" question with the caller's own token.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	pal, ok := s.principal(r)
	if !ok {
		writeAdmin(w, http.StatusUnauthorized, map[string]any{"error": "unknown token"})
		return
	}
	masked, id := "", ""
	if tok := bearer(r); tok != "" {
		masked, id = maskToken(tok), tokenID(tok)
	}
	writeAdmin(w, http.StatusOK, map[string]any{
		"user": pal.UserID, "name": pal.Name, "role": pal.Role, "workspace": pal.Workspace,
		"models": s.Policy.VisibleModels(pal), "admin": s.Policy.Roles[pal.Role].Admin,
		"token_masked": masked, "token_id": id,
	})
}

// meRotate replaces the caller's own token. The old one stops working on the next request, so the
// answer carries the new one — the console stores it immediately, and the person is told to copy.
func (s *Server) meRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdmin(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	pal, ok := s.principal(r)
	if !ok {
		writeAdmin(w, http.StatusUnauthorized, map[string]any{"error": "unknown token"})
		return
	}
	if s.MCP == nil {
		writeAdmin(w, http.StatusServiceUnavailable, map[string]any{"error": "no token store"})
		return
	}
	current := bearer(r)
	tokens := s.MCP.tokens.snapshot()
	fresh, err := newToken()
	if err != nil {
		writeAdmin(w, http.StatusInternalServerError, map[string]any{"error": "no entropy"})
		return
	}
	delete(tokens, current)
	tokens[fresh] = pal.UserID
	if err := s.MCP.tokens.Save(tokens); err != nil {
		writeAdmin(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.audit(map[string]any{"event": "token_rotated", "user": pal.UserID, "by": "self",
		"at": time.Now().UTC().Format(time.RFC3339)})
	writeAdmin(w, http.StatusOK, map[string]any{
		"token": fresh, "masked": maskToken(fresh), "user": pal.UserID,
		"note": "the previous token stops working now; this one is shown once",
	})
}

func (s *Server) adminTokens(w http.ResponseWriter, r *http.Request) {
	pal, ok := s.adminGuard(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeAdmin(w, http.StatusOK, map[string]any{"tokens": s.tokenRows()})
	case http.MethodPost:
		var body struct {
			User string `json:"user"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeAdmin(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
			return
		}
		if body.User == "" {
			writeAdmin(w, http.StatusBadRequest, map[string]any{"error": "user is required"})
			return
		}
		// Only a provisioned user gets a token: a token for an unknown id is a 401 nobody can
		// explain, and a token for a user the directory disabled is a way around the directory.
		if _, known := s.Policy.Users[body.User]; !known {
			writeAdmin(w, http.StatusNotFound, map[string]any{"error": "unknown user " + body.User})
			return
		}
		if s.MCP == nil {
			writeAdmin(w, http.StatusServiceUnavailable, map[string]any{"error": "no token store"})
			return
		}
		fresh, err := newToken()
		if err != nil {
			writeAdmin(w, http.StatusInternalServerError, map[string]any{"error": "no entropy"})
			return
		}
		tokens := s.MCP.tokens.snapshot()
		tokens[fresh] = body.User
		if err := s.MCP.tokens.Save(tokens); err != nil {
			writeAdmin(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.audit(map[string]any{"event": "token_created", "user": body.User, "by": pal.UserID,
			"at": time.Now().UTC().Format(time.RFC3339)})
		writeAdmin(w, http.StatusCreated, map[string]any{
			"token": fresh, "masked": maskToken(fresh), "user": body.User, "id": tokenID(fresh),
			"note": "shown once; the list only ever shows the mask"})
	case http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/admin/tokens/")
		if id == "" || id == r.URL.Path {
			writeAdmin(w, http.StatusBadRequest, map[string]any{"error": "token id is required"})
			return
		}
		if s.MCP == nil {
			writeAdmin(w, http.StatusServiceUnavailable, map[string]any{"error": "no token store"})
			return
		}
		tokens := s.MCP.tokens.snapshot()
		for tok, user := range tokens {
			if tokenID(tok) != id {
				continue
			}
			if tok == bearer(r) {
				// Revoking your own token here would log you out of the page you are using and
				// leave no way back in until an operator edits the file. That is what rotate is for.
				writeAdmin(w, http.StatusConflict, map[string]any{
					"error": "that is your own token; use POST /me/token/rotate"})
				return
			}
			delete(tokens, tok)
			if err := s.MCP.tokens.Save(tokens); err != nil {
				writeAdmin(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			s.audit(map[string]any{"event": "token_revoked", "user": user, "by": pal.UserID,
				"at": time.Now().UTC().Format(time.RFC3339)})
			writeAdmin(w, http.StatusOK, map[string]any{"revoked": id, "user": user})
			return
		}
		writeAdmin(w, http.StatusNotFound, map[string]any{"error": "no such token"})
	default:
		writeAdmin(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET, POST or DELETE"})
	}
}

// decodeJSON reads a bounded body: a console request is small, and an unbounded read is a way to
// turn a config page into a memory problem.
func decodeJSON(r *http.Request, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// bearer is the authorization token as presented, without the scheme.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer"))
	}
	return strings.TrimSpace(r.Header.Get("X-DSH-Token"))
}
