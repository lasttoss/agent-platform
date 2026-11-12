// The demo tool service as its own process: the gateway must not be able to answer a salary
// question on its own, so the fixtures live here, behind MCP.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/lasttoss/agent-platform/services/gateway-go/internal/demotools"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8091", "listen address")
	localPath := flag.String("local", "", "optional gitignored salary overrides")
	flag.Parse()

	if *localPath != "" {
		if err := demotools.LoadLocal(*localPath); err != nil {
			log.Fatalf("local: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", demotools.Handler())
	log.Printf("demo tool service on http://%s/mcp", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
