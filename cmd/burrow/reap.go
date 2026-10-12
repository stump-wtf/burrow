package main

import (
	"context"
	"log/slog"
	"time"

	"gitea.stump.rocks/stump.wtf/burrow/internal/store"
)

// reapInterval is how often expired rows are swept. Expiry is enforced on
// every read, so the sweep only bounds table growth and hourly is plenty.
const reapInterval = time.Hour

// reapExpired sweeps expired sessions, grants, sign-in states and MCP tokens
// once immediately — a restart must not wait an interval to shed what piled
// up — and then every interval until ctx ends. A failed sweep is logged and
// retried on the next tick: it is housekeeping, and must never take the
// server down with it.
func reapExpired(ctx context.Context, st *store.Store, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		purged, err := st.PurgeExpired(ctx)
		if err != nil {
			slog.Warn("purge expired rows", "error", err)
		} else {
			slog.Debug("purged expired rows", "sessions", purged.Sessions, "grants", purged.Grants,
				"oidc_states", purged.OIDCStates, "mcp_tokens", purged.MCPTokens)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
