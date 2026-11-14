package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	tokensPath := flag.String("tokens", "../../config/dsh-tokens.json", "MCP client tokens (token → user id)")
	rolesPath := flag.String("roles", "../../config/roles.json", "role assignments written by the console")
	addr := flag.String("addr", "127.0.0.1:8090", "listen address")
	audit := flag.String("audit", "state/audit.log", "audit log path")
	refresh := flag.Duration("refresh", 30*time.Second,
		"how often to re-probe services (health, then tools/list) and reload the user directory; 0 disables")
	auditHTTP := flag.String("audit-http", os.Getenv("AUDIT_SINK_URL"),
		"optional collector URL: each audit line is also shipped there (best effort)")
	setActive := flag.String("set-active", "",
		"enable or disable a user in the directory, id:true|false, then exit")
	checkUsers := flag.Bool("check-users", false,
		"compare the user directory with policy.json, print the diff, exit 1 on mismatch")
	flag.Parse()

	// Defaults work from the service directory and from the repo root: guessing wrong about
	// the caller's cwd is a self-inflicted startup failure.
	*policyPath = firstExisting(*policyPath, "services/gateway/config/policy.json")
	*consentPath = firstExisting(*consentPath, "config/consent.json")
	*toolsPath = firstExisting(*toolsPath, "config/tools.json")
	*mcpPath = firstExisting(*mcpPath, "config/mcp.json")
	*tokensPath = firstExisting(*tokensPath, "config/dsh-tokens.json")
	*rolesPath = firstExisting(*rolesPath, "config/roles.json")

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
	roles, err := NewRoleStore(*rolesPath)
	if err != nil {
		log.Fatalf("roles: %v", err)
	}
	srv.Roles = roles
	fmt.Printf("roles: %d assignment(s) from %s — /admin/* needs a role with admin:true\n",
		len(roles.Snapshot()), *rolesPath)
	if err := srv.RefreshDirectory(reg); err != nil {
		log.Printf("user directory unavailable (%v) — policy.json decides who may act", err)
	} else {
		fmt.Printf("user directory loaded (%d users)\n", len(srv.dir))
	}
	if *checkUsers {
		report, err := srv.CheckUsers(reg)
		if err != nil {
			log.Fatalf("check-users: %v", err)
		}
		raw, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(raw))
		if match, _ := report["match"].(bool); !match {
			os.Exit(1)
		}
		return
	}
	if *setActive != "" {
		parts := strings.SplitN(*setActive, ":", 2)
		if len(parts) != 2 || (parts[1] != "true" && parts[1] != "false") {
			log.Fatalf("set-active wants id:true or id:false, got %q", *setActive)
		}
		out, err := srv.SetUserActive(reg, parts[0], parts[1] == "true")
		if err != nil {
			log.Fatalf("set-active: %v", err)
		}
		raw, _ := json.Marshal(out)
		fmt.Println(string(raw))
		return
	}
	if *refresh > 0 {
		// Health first, then the MCP tool list: a service that comes up after the gateway
		// becomes usable without a restart, and one that dies stops being offered.
		go func() {
			for range time.Tick(*refresh) {
				rep := reg.Refresh()
				if err := srv.RefreshDirectory(reg); err != nil {
					log.Printf("refresh: user directory: %v", err)
				}
				if len(rep.Added) > 0 || len(rep.Removed) > 0 {
					log.Printf("refresh: +%v -%v", rep.Added, rep.Removed)
				}
				for _, err := range rep.Errors {
					log.Printf("refresh: %v", err)
				}
			}
		}()
	}
	srv.StartAuditSink(*auditHTTP)
	if *auditHTTP != "" {
		fmt.Println("audit sink:", *auditHTTP)
	}
	if _, err := os.ReadFile(*tokensPath); err != nil {
		// No token file means /mcp stays off: an unauthenticated tool endpoint is worse than
		// an absent one, and silence about it is worst of all.
		log.Printf("mcp front disabled: %v", err)
	} else {
		ts, err := NewTokenSet(*tokensPath)
		if err != nil {
			log.Fatalf("tokens: %v", err)
		}
		srv.MCP = NewMCPFront(srv, ts)
		fmt.Printf("mcp front on /mcp — %d token(s), identity from transport, hot reload on file change\n", ts.Count())
	}
	srv.AdapterToken = os.Getenv("ADAPTER_TOKEN")
	if srv.AdapterToken != "" {
		fmt.Println("adapter token required on /v1/respond and /v1/whoami")
	}
	log.Fatal(http.ListenAndServe(*addr, srv.Routes()))
}
