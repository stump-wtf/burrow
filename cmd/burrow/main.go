// burrow is a self-hosted, OpenWork-compatible control plane: OIDC sign-in,
// org, Library skills, and the MCP endpoint that delivers skills to the agent.
//
// Governing: ADR-0001 (clean-room implementation of the client contract).
// The contract itself is docs/client-contract.md.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"gitea.stump.rocks/stump.wtf/burrow/internal/buildinfo"
)

func main() {
	addr := os.Getenv("BURROW_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "burrow %s — stage A in progress\n", buildinfo.Version)
	})

	slog.Info("burrow listening", "addr", addr, "version", buildinfo.Version)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}
