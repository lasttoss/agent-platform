package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// The gateway keeps three things: who may call, what has been called, and what answer can be
// reused. Everything a caller can actually *do* is an MCP service — including the model.
func firstExisting(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return paths[0]
}

func main() {
	policyPath := flag.String("policy", "../gateway/config/policy.json", "policy file")
	consentPath := flag.String("consent", "../../config/consent.json", "consent file")
	localPath := flag.String("local", "../../config/local.json", "gitignored local overrides")
	toolsPath := flag.String("tools", "../../config/tools.json", "tool policy metadata")
	mcpPath := flag.String("mcp", "../../config/mcp.json", "MCP services")
	addr := flag.String("addr", "127.0.0.1:8090", "listen address")
	audit := flag.String("audit", "state/audit.log", "audit log path")
	flag.Parse()

	// Defaults work from the service directory and from the repo root: guessing wrong about
	// the caller's cwd is a self-inflicted startup failure.
	*policyPath = firstExisting(*policyPath, "services/gateway/config/policy.json")
	*consentPath = firstExisting(*consentPath, "config/consent.json")
	*toolsPath = firstExisting(*toolsPath, "config/tools.json")
	*mcpPath = firstExisting(*mcpPath, "config/mcp.json")

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

	meta, err := LoadToolsMeta(*toolsPath)
	if err != nil {
		log.Fatalf("tools: %v", err)
	}
	confs, err := LoadServers(*mcpPath)
	if err != nil {
		log.Fatalf("mcp: %v", err)
	}
	reg := NewRegistry(meta, confs)
	for _, err := range reg.Discover() {
		// A missing service is not fatal: its tools simply do not exist for this run, which is
		// the same outcome as never having written them down. Say so loudly, though.
		log.Printf("mcp: %v (tools from that server are unavailable)", err)
	}
	advertised := 0
	for name := range reg.Meta {
		if reg.Advertises(name) {
			advertised++
		}
	}
	fmt.Printf("gateway-go on http://%s — %d tools advertised by %d service(s), model service: %v\n",
		*addr, advertised, len(confs), reg.Model != nil)

	if dir := filepath.Dir(*audit); dir != "." {
		os.MkdirAll(dir, 0o755)
	}
	srv := NewServer(pol, consent, reg, *audit)
	log.Fatal(http.ListenAndServe(*addr, srv.Routes()))
}
