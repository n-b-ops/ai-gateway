// Package coalesce implements cold-anchor coalescing: when N parallel requests
// share an unseen prefix anchor, collapse them so only the first (leader) pays
// the cold-miss price. Followers block until the leader warms the cache, then
// proceed on a warm prefix.
//
// This mirrors Permafrost's Coalescer but at the gateway level, working with
// core.Request rather than raw HTTP bodies.
package coalesce

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// Status describes the result of acquiring a coalescer slot.
type Status int

const (
	// StatusPass means the anchor is known-warm — proceed immediately.
	StatusPass Status = iota
	// StatusLeader means this is the first request on an unseen anchor.
	// Proceed immediately to warm the cache.
	StatusLeader
	// StatusFollower means another request (the leader) is already warming
	// this anchor. Block until the leader releases.
	StatusFollower
)

func (s Status) String() string {
	switch s {
	case StatusPass:
		return "pass"
	case StatusLeader:
		return "leader"
	case StatusFollower:
		return "follower"
	default:
		return "unknown"
	}
}

// anchorGate controls access for one anchor fingerprint.
type anchorGate struct {
	warm    bool
	gate    chan struct{} // closed when followers can proceed
	expires time.Time
	created time.Time
}

// Config holds coalescer configuration.
type Config struct {
	Enabled   bool
	SettleMs  int // extra wait after leader release for async cache write
	TimeoutS  int // follower deadlock guard
	Cap       int // max tracked anchors
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:  false,
		SettleMs: 2500,
		TimeoutS: 30,
		Cap:      1024,
	}
}

// Coalescer coordinates parallel requests to avoid cold-cache fan-out.
type Coalescer struct {
	mu      sync.Mutex
	anchors map[string]*anchorGate
	order   []string
	Cfg     Config
}

// New creates a new Coalescer with the given configuration.
func New(cfg Config) *Coalescer {
	if cfg.Cap <= 0 {
		cfg.Cap = 1024
	}
	if cfg.SettleMs <= 0 {
		cfg.SettleMs = 2500
	}
	if cfg.TimeoutS <= 0 {
		cfg.TimeoutS = 30
	}
	return &Coalescer{
		anchors: make(map[string]*anchorGate, cfg.Cap),
		order:   make([]string, 0, cfg.Cap),
		Cfg:     cfg,
	}
}

// Acquire checks the coalescer for the given anchor fingerprint. Returns:
//
//	StatusPass + nil gate    — anchor is warm, proceed immediately
//	StatusLeader + nil gate  — unseen anchor, proceed (you're the leader)
//	StatusFollower + gate    — leader in flight, block on gate
//
// If status is StatusFollower, the caller MUST wait on gate.Wait(timeout).
func (c *Coalescer) Acquire(fp string) (Status, *Gate) {
	if fp == "" {
		return StatusPass, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Clean up expired entries.
	now := time.Now()
	c.evictExpired(now)

	ag, exists := c.anchors[fp]
	if exists {
		// LRU touch.
		for i, k := range c.order {
			if k == fp {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
		c.order = append(c.order, fp)

		if ag.warm {
			return StatusPass, nil
		}
		// Leader is still in flight — follower.
		return StatusFollower, &Gate{inner: ag.gate}
	}

	// New anchor — become leader.
	gate := make(chan struct{})
	ag = &anchorGate{
		gate:    gate,
		created: now,
		expires: now.Add(time.Duration(c.Cfg.TimeoutS) * time.Second),
	}

	// LRU eviction.
	for len(c.order) >= c.Cfg.Cap {
		delete(c.anchors, c.order[0])
		c.order = c.order[1:]
	}

	c.anchors[fp] = ag
	c.order = append(c.order, fp)
	return StatusLeader, nil
}

// Release opens the gate for followers. Call this when the leader receives
// its first response byte (the cache is being written).
func (c *Coalescer) Release(fp string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ag, exists := c.anchors[fp]
	if !exists || ag.warm {
		return
	}
	// Close the gate so all followers unblock.
	close(ag.gate)
	ag.warm = true
}

// Fail removes the anchor entry, allowing the next request to become a fresh
// leader. Call this when the leader's request fails.
func (c *Coalescer) Fail(fp string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.anchors, fp)
	for i, k := range c.order {
		if k == fp {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// evictExpired removes entries past their expiration time.
// Must be called with mu held.
func (c *Coalescer) evictExpired(now time.Time) {
	keep := make([]string, 0, len(c.order))
	for _, fp := range c.order {
		ag, exists := c.anchors[fp]
		if !exists {
			continue
		}
		if now.After(ag.expires) {
			delete(c.anchors, fp)
			continue
		}
		keep = append(keep, fp)
	}
	c.order = keep
}

// Stats returns diagnostic information.
func (c *Coalescer) Stats() map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	warm := 0
	pending := 0
	for _, ag := range c.anchors {
		if ag.warm {
			warm++
		} else {
			pending++
		}
	}

	return map[string]interface{}{
		"total":   len(c.anchors),
		"warm":    warm,
		"pending": pending,
		"cap":     c.Cfg.Cap,
		"now":     now.Format(time.RFC3339),
	}
}

// Gate wraps a channel for follower blocking. Followers call Wait() which
// blocks until the leader releases the gate or the timeout elapses.
type Gate struct {
	inner chan struct{}
}

// Wait blocks until the gate is opened or the timeout expires. Returns true
// if the gate was opened (leader succeeded), false if timeout.
func (g *Gate) Wait(timeout time.Duration) bool {
	if g == nil || g.inner == nil {
		return true
	}
	select {
	case <-g.inner:
		return true
	case <-time.After(timeout):
		return false
	}
}

// AnchorFingerprint computes a stable hash of the cacheable prefix of a
// request (tools + system). Two requests with the same fingerprint share
// a byte-identical prefix and will hit the upstream cache for the anchor.
func AnchorFingerprint(req *core.Request) string {
	payload := struct {
		Tools  []core.Tool    `json:"tools"`
		System []core.Message `json:"system"`
	}{
		Tools: req.Tools,
	}

	for _, msg := range req.Messages {
		if msg.Role == core.RoleSystem {
			payload.System = append(payload.System, msg)
		}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(raw)
	return fmt.Sprintf("%x", h[:8])
}
