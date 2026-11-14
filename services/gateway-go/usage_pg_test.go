package main

import (
	"os"
	"testing"
)

// Two replicas must add up, not overwrite each other. This needs a real Postgres, so it is opt-in:
//
//	USAGE_TEST_DSN='postgresql://postgres:devpassword@127.0.0.1:5433/agent?sslmode=disable' go test -run PgUsage
func TestPgUsageSumsTwoReplicas(t *testing.T) {
	dsn := os.Getenv("USAGE_TEST_DSN")
	if dsn == "" {
		t.Skip("set USAGE_TEST_DSN to run this against a real postgres")
	}
	mk := func() (*pgUsage, *Server) {
		pg, err := newPgUsage(dsn)
		if err != nil {
			t.Fatal(err)
		}
		pol := &Policy{Quota: Quota{RequestsPerDay: 1000, TokensPerDay: 1000}, Users: map[string]User{},
			Chats: map[string]ChatConf{"dm": {Tools: []string{"*"}}}, Roles: map[string]RoleConf{}}
		s := NewServer(pol, nil, nil, os.DevNull)
		return pg, s
	}
	aPg, a := mk()
	bPg, b := mk()
	defer aPg.db.Close()
	defer bPg.db.Close()

	// Clean the row for a deterministic run.
	if _, err := aPg.db.Exec(`DELETE FROM usage_counters WHERE user_id = 'pgtest'`); err != nil {
		t.Fatal(err)
	}
	a.chargeRequest("pgtest")
	a.chargeRequest("pgtest")
	if err := aPg.save(a); err != nil {
		t.Fatal(err)
	}
	b.chargeRequest("pgtest")
	if err := bPg.save(b); err != nil {
		t.Fatal(err)
	}
	if _, err := aPg.load(a); err != nil {
		t.Fatal(err)
	}
	if got := a.usage["pgtest"]["requests"]; got != 3 {
		t.Fatalf("two replicas spent 3 requests, database says %d", got)
	}

	// A restart must not re-add what it just loaded: this was a real double count, found live.
	cPg, c := mk()
	defer cPg.db.Close()
	if _, err := cPg.load(c); err != nil {
		t.Fatal(err)
	}
	if err := cPg.save(c); err != nil {
		t.Fatal(err)
	}
	var stored int
	if err := cPg.db.QueryRow(`SELECT requests FROM usage_counters WHERE day=$1 AND user_id='pgtest'`,
		today()).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 3 {
		t.Fatalf("a restart doubled the counters: database says %d", stored)
	}
}
