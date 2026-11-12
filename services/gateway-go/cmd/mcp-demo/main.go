// The demo tool service as its own process: the gateway must not be able to answer a salary
// question on its own, so the fixtures live here, behind MCP.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/lasttoss/agent-platform/services/gateway-go/internal/demotools"
)

func main() {
	defaultAddr := "127.0.0.1:8091"
	if v := os.Getenv("ADDR"); v != "" { // containers: ADDR=0.0.0.0:8091
		defaultAddr = v
	}
	addr := flag.String("addr", defaultAddr, "listen address")
	localPath := flag.String("local", "", "optional gitignored salary overrides")
	// A real tool service is not infinitely parallel: it has a worker pool and a call that takes
	// time. Both are flags so the gateway can be measured against a downstream that pushes back.
	workers := flag.Int("workers", 0, "bounded worker pool size (0 = unbounded)")
	delay := flag.Duration("delay", 0, "artificial latency per call, e.g. 50ms")
	flag.Parse()
	demotools.Configure(*workers, *delay)

	if *localPath != "" {
		if err := demotools.LoadLocal(*localPath); err != nil {
			log.Fatalf("local: %v", err)
		}
	}
	mux := http.NewServeMux()
	handler := demotools.Handler()
	mux.Handle("/mcp", handler)
	mux.Handle("/health", handler) // answered by the same handler: GET /health → {"ok":true}
	log.Printf("demo tool service on http://%s/mcp", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
