// main.go — the Go forge server entrypoint.
//
// A pure HTTP server: it binds a port and serves the forge. The port comes
// from the --port flag, else the FORGE_PORT environment variable, else 0
// (ephemeral — the kernel picks one). It listens on all interfaces
// (":port") so the env can dial it at the pod's address, and prints the
// bound port to stdout as its first line (`FORGE_PORT=<n>`) so the Node
// launcher (main.ts) can register it. All other logging goes to stderr.
//
// Lifecycle is owned by main.ts: it spawns this binary, registers the
// port, polls the env's /done, then kills this process.

package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
)

func main() {
	portFlag := flag.Int("port", 0, "port to listen on (0 = ephemeral)")
	flag.Parse()

	port := *portFlag
	if port == 0 {
		if v := os.Getenv("FORGE_PORT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				port = n
			}
		}
	}

	store := NewStore()
	handler := makeHandler(store)

	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	// The one stdout line main.ts parses for the port.
	fmt.Printf("FORGE_PORT=%d\n", actualPort)
	fmt.Fprintf(os.Stderr, "[forge] listening on port %d\n", actualPort)
	fmt.Fprintf(os.Stderr, "[forge] github-forge-go 1.0.0 (git smart HTTP, REST v3, GraphQL v4, MCP)\n")

	server := &http.Server{Handler: handler}
	// Keep-alive tuning: the suite reuses one connection for 2000
	// sequential reads; short idle timeouts would force reconnects.
	server.IdleTimeout = 0 // no idle timeout; the process is killed at /done
	if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
		os.Exit(1)
	}
}
