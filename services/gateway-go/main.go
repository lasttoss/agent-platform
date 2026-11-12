package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// Same policy file, same answer, different language. The port exists to check that the
// decisions live in the data and the algorithm — not in Python.
func main() {
	policyPath := flag.String("policy", "../gateway/config/policy.json", "policy file")
	consentPath := flag.String("consent", "../../config/consent.json", "consent file")
	localPath := flag.String("local", "../../config/local.json", "gitignored local overrides")
	addr := flag.String("addr", "127.0.0.1:8090", "listen address")
	audit := flag.String("audit", "state/audit.log", "audit log path")
	flag.Parse()

	pol, err := LoadPolicy(*policyPath)
	if err != nil {
		log.Fatalf("policy: %v", err)
	}
	consent, err := LoadConsent(*consentPath)
	if err != nil {
		log.Fatalf("consent: %v", err)
	}
	local, err := LoadLocal(*localPath)
	if err != nil {
		log.Fatalf("local: %v", err)
	}
	ApplyLocal(pol, consent, local)

	if dir := filepath.Dir(*audit); dir != "." {
		os.MkdirAll(dir, 0o755)
	}
	srv := NewServer(pol, consent, *audit)
	fmt.Printf("gateway-go on http://%s (policy: %s)\n", *addr, *policyPath)
	log.Fatal(http.ListenAndServe(*addr, srv.Routes()))
}
