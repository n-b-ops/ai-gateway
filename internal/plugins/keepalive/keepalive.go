// Package keepalive implements an after_request plugin that prevents upstream
// cache TTL eviction by replaying the most recent request after idle periods.
//
// Register with a blank import:
//
//	_ "github.com/ferro-labs/ai-gateway/internal/plugins/keepalive"
//
// Design mirrors Permafrost's keepalive, generalized for any upstream provider.
// Each session slot stores the last request. After idle >= interval_s, the
// request is replayed via the gateway itself to keep the upstream cache warm.
// An X-Keepalive header prevents recursive re-storage of keepalive replays.
//
// Config:
//
//	plugins:
//	  - name: keepalive
//	    type: utility
//	    stage: after_request
//	    enabled: true
//	    config:
//	      interval_s: 1800      # 30 min idle before keepalive fires
//	      idle_stop_s: 36000    # Stop after 10 hours of idle
//	      slot_cap: 8           # Max concurrent sessions
//	      gateway_url: ""       # Self-call URL (default: http://localhost:8080)
//	      auth_header: ""       # Auth header value for self-call (e.g. "Bearer sk-...")
package keepalive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

const keepaliveHeader = "X-Keepalive"

// context keys for passing routing hints.
type keepaliveCtxKey struct{}
type requestPathCtxKey struct{}

// isAnthropicBody detects whether a serialized JSON request body is in
// Anthropic Messages API format (has "system" field and no "n"/"seed" fields).
func isAnthropicBody(body []byte) bool {
	var raw struct {
		System   any    `json:"system"`
		Messages []any  `json:"messages"`
		MaxTokens int   `json:"max_tokens"`
		Stream   bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return false
	}
	// Anthropic format has messages array (always) and uses max_tokens (required).
	// OpenAI format uses "messages" too but doesn't have top-level "system".
	// The presence of a non-nil "system" field at top level is a strong signal
	// for Anthropic format.
	return raw.System != nil && raw.Messages != nil
}

func init() {
	plugin.RegisterFactory("keepalive", func() plugin.Plugin {
		return &Keepalive{}
	})
}

// keepaliveSlot holds the state for one session/conversation.
type keepaliveSlot struct {
	key      string
	body     []byte // serialized JSON of the last request
	path     string // the HTTP path (/v1/chat/completions or /v1/messages)
	headers  http.Header
	lastReal time.Time
	lastFire time.Time
}

// Keepalive is an after_request plugin that replays idle requests.
type Keepalive struct {
	mu         sync.Mutex
	slots      map[string]*keepaliveSlot
	order      []string
	slotCap    int
	interval   time.Duration
	idleStop   time.Duration
	gatewayURL string
	authHeader string
	httpClient *http.Client
	tickerDone chan struct{}
}

// Name returns the plugin identifier.
func (k *Keepalive) Name() string { return "keepalive" }

// Type returns the plugin lifecycle hook type.
func (k *Keepalive) Type() plugin.PluginType { return plugin.TypeTransform }

// Init configures the keepalive plugin.
func (k *Keepalive) Init(config map[string]interface{}) error {
	intervalS := 1800.0
	if v, ok := config["interval_s"].(float64); ok && v > 0 {
		intervalS = v
	}
	idleStopS := 36000.0
	if v, ok := config["idle_stop_s"].(float64); ok {
		idleStopS = v
	}
	slotCap := 8
	if v, ok := config["slot_cap"].(float64); ok {
		slotCap = int(v)
	}
	gatewayURL := "http://localhost:8080"
	if v, ok := config["gateway_url"].(string); ok && v != "" {
		gatewayURL = v
	}
	var authHeader string
	if v, ok := config["auth_header"].(string); ok {
		authHeader = v
	}

	k.slots = make(map[string]*keepaliveSlot, slotCap)
	k.order = make([]string, 0, slotCap)
	k.slotCap = slotCap
	k.interval = time.Duration(intervalS) * time.Second
	k.idleStop = time.Duration(idleStopS) * time.Second
	k.gatewayURL = gatewayURL
	k.authHeader = authHeader
	k.httpClient = &http.Client{Timeout: 10 * time.Minute}
	k.tickerDone = make(chan struct{})

	go k.loop()
	return nil
}

// sessionKey returns a stable session identifier from the request.
func sessionKey(req *core.Request) string {
	h := sha256.Sum256([]byte(req.Model))
	return fmt.Sprintf("%x", h[:8])
}

// Execute stores the current request for potential keepalive replay.
// Skips requests that already carry the X-Keepalive marker (set by the
// keepalive self-call) to avoid infinite recursion.
func (k *Keepalive) Execute(ctx context.Context, pctx *plugin.Context) error {
	if pctx.Request == nil {
		return nil
	}

	// Skip keepalive replays themselves — check the request context.
	if ctx != nil {
		if v := ctx.Value(keepaliveCtxKey{}); v != nil {
			return nil
		}
	}

	req := pctx.Request
	key := sessionKey(req)
	now := time.Now()

	// Serialize the request for replay.
	body, err := json.Marshal(req)
	if err != nil {
		return nil // Non-fatal: can't store, skip.
	}

	// Determine the replay path: check the context for format hint,
	// then fall back to detecting Anthropic format from the body.
	path := "/v1/chat/completions"
	if ctx != nil {
		if p, ok := ctx.Value(requestPathCtxKey{}).(string); ok && p != "" {
			path = p
		}
	}
	// If no explicit path, detect from the JSON body.
	if path == "/v1/chat/completions" && isAnthropicBody(body) {
		path = "/v1/messages"
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	slot, exists := k.slots[key]
	if !exists {
		for len(k.order) >= k.slotCap {
			delete(k.slots, k.order[0])
			k.order = k.order[1:]
		}
		slot = &keepaliveSlot{key: key}
		k.slots[key] = slot
	} else {
		for i, kk := range k.order {
			if kk == key {
				k.order = append(k.order[:i], k.order[i+1:]...)
				break
			}
		}
	}
	k.order = append(k.order, key)

	slot.body = body
	slot.path = path
	slot.headers = http.Header{
		keepaliveHeader: {"1"},
	}
	if k.authHeader != "" {
		slot.headers.Set("Authorization", k.authHeader)
	}
	slot.lastReal = now

	return nil
}

// fire replays the stored request to the gateway itself, keeping the
// upstream cache warm. Errors are logged but never surfaced.
func (k *Keepalive) fire(slot *keepaliveSlot) {
	url := k.gatewayURL + slot.path
	r, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(slot.body))
	if err != nil {
		slog.Warn("keepalive: failed to create request", "session", slot.key, "error", err)
		return
	}
	r.Header.Set("Content-Type", "application/json")
	for kk, vv := range slot.headers {
		for _, v := range vv {
			r.Header.Set(kk, v)
		}
	}

	slog.Debug("keepalive: firing", "session", slot.key, "url", url)
	resp, err := k.httpClient.Do(r)
	if err != nil {
		slog.Warn("keepalive: request failed", "session", slot.key, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain the response body so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		slog.Warn("keepalive: upstream error", "session", slot.key, "status", resp.StatusCode)
	} else {
		slog.Debug("keepalive: OK", "session", slot.key, "status", resp.StatusCode)
	}
}

// loop is the background keepalive ticker.
func (k *Keepalive) loop() {
	tickInterval := time.Duration(k.interval.Nanoseconds()/4) * time.Nanosecond
	if tickInterval < 500*time.Millisecond {
		tickInterval = 500 * time.Millisecond
	}
	if tickInterval > 30*time.Second {
		tickInterval = 30 * time.Second
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-k.tickerDone:
			return
		case now := <-ticker.C:
			k.mu.Lock()
			for _, slot := range k.slots {
				if k.isDue(slot, now) {
					slot.lastFire = now
					s := slot
					go k.fire(s)
				}
			}
			k.mu.Unlock()
		}
	}
}

func (k *Keepalive) isDue(slot *keepaliveSlot, now time.Time) bool {
	idle := now.Sub(slot.lastReal)
	if idle < k.interval {
		return false
	}
	if idle >= k.idleStop {
		return false
	}
	if now.Sub(slot.lastFire) < k.interval {
		return false
	}
	return true
}
