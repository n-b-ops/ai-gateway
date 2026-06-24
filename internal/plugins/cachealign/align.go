// Package cachealign implements a before_request plugin that stabilises the
// cacheable prefix of LLM requests so upstream prefix-caching (DeepSeek,
// Anthropic, OpenAI, etc.) can hit consistently across turns.
//
// Register with a blank import:
//
//	_ "github.com/ferro-labs/ai-gateway/internal/plugins/cachealign"
//
// The pipeline, executed in order on each providers.Request before the
// provider call:
//
//  1. Sort tools — deterministic order regardless of MCP server timing.
//  2. Strip cache_control — remove Anthropic cache_control markers that
//     DeepSeek ignores but that shift byte positions each turn.
//  3. Stabilize metadata — pin volatile billing nonces (cch=) to constants.
//  4. Strip client noise — remove Claude Code's TodoWrite reminder system
//     messages that vary but carry no semantic content.
//  5. Freeze volatile env — pin the first-seen env block (cwd, date, git
//     status) into the cached anchor; emit only changed lines as a delta
//     on the last user turn. Falls back to relocation (move the whole env
//     block to the tail) when freeze isn't possible.
//
// Config:
//
//	plugins:
//	  - name: cachealign
//	    type: transform
//	    stage: before_request
//	    enabled: true
//	    config:
//	      freeze_env: true        # default: true
//	      freeze_store_cap: 1024  # max concurrent sessions
//	      strip_cache_control: true
//	      strip_todowrite: true
//	      stabilize_metadata: true
package cachealign

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

func init() {
	plugin.RegisterFactory("cachealign", func() plugin.Plugin {
		return &CacheAlign{}
	})
}

var (
	// Regex patterns ported from permafrost_align.py.
	isoDateTimeRE = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(?::\d{2})?\b`)
	uuidRE        = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	dateRE        = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)

	// Claude Code injects "The TodoWrite tool hasn't been used recently..." as a
	// system-role message after several tool loops. It carries no user/project
	// context and varies, creating new tail prefixes.
	todoWritePrefix = "The TodoWrite tool hasn't been used recently."

	// Claude Code's billing-telemetry block carries a per-request `cch=<nonce>`.
	billingMarker = "x-anthropic-billing-header"
	cchRE         = regexp.MustCompile(`(cch=)[^;\s]*`)
)

// CacheAlign is a before_request transform plugin that stabilises the
// cacheable prefix of LLM requests.
type CacheAlign struct {
	freezeStore       *freezeStore
	freezeEnv         bool
	stripCacheControl bool
	stripTodoWrite    bool
	stabilizeMetadata bool
}

// Name returns the plugin identifier.
func (ca *CacheAlign) Name() string { return "cachealign" }

// Type returns the plugin lifecycle hook type.
func (ca *CacheAlign) Type() plugin.PluginType { return plugin.TypeTransform }

// Init configures the plugin from the provided options map.
func (ca *CacheAlign) Init(config map[string]interface{}) error {
	// freeze_env: default true
	if v, ok := config["freeze_env"].(bool); ok {
		ca.freezeEnv = v
	} else {
		ca.freezeEnv = true
	}

	// freeze_store_cap: default 1024
	cap := 1024
	if v, ok := config["freeze_store_cap"].(float64); ok {
		cap = int(v)
	}
	ca.freezeStore = newFreezeStore(cap)

	// strip_cache_control: default true
	if v, ok := config["strip_cache_control"].(bool); ok {
		ca.stripCacheControl = v
	} else {
		ca.stripCacheControl = true
	}

	// strip_todowrite: default true
	if v, ok := config["strip_todowrite"].(bool); ok {
		ca.stripTodoWrite = v
	} else {
		ca.stripTodoWrite = true
	}

	// stabilize_metadata: default true
	if v, ok := config["stabilize_metadata"].(bool); ok {
		ca.stabilizeMetadata = v
	} else {
		ca.stabilizeMetadata = true
	}

	return nil
}

// Execute runs the cache alignment pipeline on the request before it reaches
// the provider.
func (ca *CacheAlign) Execute(_ context.Context, pctx *plugin.Context) error {
	if pctx.Request == nil {
		return nil
	}

	req := pctx.Request

	// Step 1: Sort tools for deterministic order.
	sortTools(req)

	// Step 2: Strip cache_control markers from messages.
	if ca.stripCacheControl {
		stripCacheControlFromRequest(req)
	}

	// Step 3: Stabilize billing metadata (cch= nonce).
	if ca.stabilizeMetadata {
		stabilizeMetadataInRequest(req)
	}

	// Step 4: Strip TodoWrite reminder system messages.
	if ca.stripTodoWrite {
		stripTodoWriteReminders(req)
	}

	// Step 5: Freeze volatile env block (or relocate as fallback).
	if ca.freezeEnv {
		ca.freezeVolatileEnv(req)
	} else {
		relocateVolatileEnv(req)
	}

	return nil
}

// --- Pipeline steps ---

// stripCacheControlFromRequest removes all cache_control fields from the
// request's message content and tool definitions. DeepSeek's automatic cache
// does not read these markers, and CC slides them to the most recent turn
// each request, so their byte positions drift.
func stripCacheControlFromRequest(req *core.Request) {
	for i := range req.Messages {
		msg := &req.Messages[i]
		// If the message has ContentParts, cache_control may be a nested field
		// in the JSON. Since core.Request stores content as string or
		// ContentPart, we strip it from the string content and any parts.
		msg.Content = stripCacheControlJSON(msg.Content)
	}

	// Tools: strip cache_control from function parameters JSON.
	for i := range req.Tools {
		req.Tools[i].Function.Parameters = stripCacheControlJSONBytes(req.Tools[i].Function.Parameters)
	}
}

// stripCacheControlJSON removes cache_control keys from a JSON string.
func stripCacheControlJSON(content string) string {
	// Match patterns like: ,"cache_control":{"type":"ephemeral"}
	// or {"cache_control":{...} inside JSON objects, stripping the key-value
	// pair. Handles leading comma, trailing comma, and adjacent spacing.
	re := regexp.MustCompile(`,?\s*"cache_control"\s*:\s*\{[^}]*\}\s*,?`)
	return re.ReplaceAllString(content, "")
}

func stripCacheControlJSONBytes(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	re := regexp.MustCompile(`,?\s*"cache_control"\s*:\s*\{[^}]*\}\s*,?`)
	result := re.ReplaceAll(raw, nil)
	return json.RawMessage(result)
}

// stabilizeMetadataInRequest pins Claude Code's per-request billing nonce
// (cch=<nonce>) to a constant value, preventing a 5-char nonce change at
// the front of the prefix from invalidating all cached system tokens.
func stabilizeMetadataInRequest(req *core.Request) {
	for i := range req.Messages {
		msg := &req.Messages[i]
		if msg.Role == core.RoleSystem && strings.Contains(msg.Content, billingMarker) {
			msg.Content = cchRE.ReplaceAllString(msg.Content, "${1}permafrost")
		}
	}
}

// stripTodoWriteReminders removes Claude Code's non-semantic TodoWrite nudge
// system messages from the messages array.
func stripTodoWriteReminders(req *core.Request) {
	keep := make([]core.Message, 0, len(req.Messages))
	for _, msg := range req.Messages {
		if msg.Role == core.RoleSystem && strings.HasPrefix(strings.TrimSpace(msg.Content), todoWritePrefix) {
			continue
		}
		keep = append(keep, msg)
	}
	if len(keep) < len(req.Messages) {
		req.Messages = keep
	}
}

// freezeVolatileEnv pins the first-seen env block into the system prompt so it
// is cached, and emits only changed lines as a delta on the last user turn.
func (ca *CacheAlign) freezeVolatileEnv(req *core.Request) {
	idx, curText := findEnvBlock(req)
	if idx < 0 {
		relocateVolatileEnv(req) // Fallback: stateless relocation.
		return
	}

	key := lineageKey(req)
	frozen := ca.freezeStore.freeze(key, curText)

	// Pin the anchor to the frozen snapshot.
	if frozen != curText {
		req.Messages[idx].Content = frozen
	}

	// Emit delta if anything changed since the snapshot.
	delta := computeDelta(frozen, curText)
	if len(delta) > 0 {
		appendToLastUserMessage(req, envUpdateBlock(delta))
	}
}

// relocateVolatileEnv moves the env block out of the system prompt and
// re-attaches it to the last user turn (wrapped in a marker), so it stops
// resetting the cache prefix every time a file changes or the date advances.
func relocateVolatileEnv(req *core.Request) {
	for i, msg := range req.Messages {
		if msg.Role == core.RoleSystem && looksLikeEnvBlock(msg.Content) && hasVolatileTokens(msg.Content) {
			// Remove from system.
			req.Messages = append(req.Messages[:i], req.Messages[i+1:]...)

			// Re-attach to last user message.
			relocated := fmt.Sprintf("<permafrost:relocated-context>\n%s\n</permafrost:relocated-context>", msg.Content)
			if !appendToLastUserMessage(req, relocated) {
				// No user turn to attach to — restore to system.
				req.Messages = append(req.Messages[:i], append([]core.Message{msg}, req.Messages[i:]...)...)
			}
			return
		}
	}
}
