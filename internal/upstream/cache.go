// The upstream tool cache.
//
// search_capabilities asks every usable connection what it offers, and an agent
// searches on every prompt. Asking the upstream servers again for each of those
// would put their latency in front of every turn of the agent, and would send a
// credential over and over for an answer that changes when an operator edits a
// connection, not when a member asks. So a tool list is trusted for a window
// (BURROW_MCP_TOOLS_TTL, 60s by default), the same shape ADR-0004 chose for a
// gateway's model menu.
//
// The cache key carries the credential's fingerprint, never the credential: two
// members of a per-member connection share a row but never each other's tool
// list, and a member who changes their key stops seeing the list their old key
// produced.
package upstream

import (
	"sync"
	"time"
)

// ToolCache holds tool lists for a window. Its zero value caches nothing.
type ToolCache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]cachedTools
}

type cachedTools struct {
	tools     []Tool
	fetchedAt time.Time
}

// NewToolCache builds a cache that trusts an entry for ttl.
func NewToolCache(ttl time.Duration) *ToolCache {
	if ttl <= 0 {
		return nil
	}
	return &ToolCache{ttl: ttl, entries: map[string]cachedTools{}}
}

// Get returns a cached list when one is still trusted.
func (c *ToolCache) Get(key string, now time.Time) ([]Tool, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || now.Sub(entry.fetchedAt) >= c.ttl {
		return nil, false
	}
	return entry.tools, true
}

// Put records a list as of now.
func (c *ToolCache) Put(key string, tools []Tool, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]cachedTools{}
	}
	c.entries[key] = cachedTools{tools: tools, fetchedAt: now}
}

// Forget drops every entry of one connection, which is what an operator's edit
// of that connection needs: the next search asks the server they just changed.
func (c *ToolCache) Forget(connectionID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if hasPrefixConn(key, connectionID) {
			delete(c.entries, key)
		}
	}
}

// CacheKey is the key Get and Put take: one connection, one credential.
func CacheKey(connectionID, credentialFingerprint string) string {
	return connectionID + "|" + credentialFingerprint
}

func hasPrefixConn(key, connectionID string) bool {
	if len(connectionID) >= len(key) {
		return false
	}
	return key[:len(connectionID)] == connectionID && key[len(connectionID)] == '|'
}
