package cachealign

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// freezeStore is a thread-safe LRU cache that stores frozen env-block snapshots
// keyed by lineage fingerprint (stable part of the request prefix). Each
// session/conversation gets its own snapshot; subsequent turns of the same
// session reuse the frozen copy, keeping the cacheable anchor byte-stable.
type freezeStore struct {
	mu    sync.Mutex
	snap  map[string]string
	order []string
	cap   int
}

func newFreezeStore(cap int) *freezeStore {
	if cap <= 0 {
		cap = 1024
	}
	return &freezeStore{
		snap:  make(map[string]string, cap),
		order: make([]string, 0, cap),
		cap:   cap,
	}
}

// freeze returns the frozen snapshot for the given lineage key. On first
// encounter, the current text is stored as the snapshot and returned. On
// subsequent calls, the stored snapshot is returned (which may differ from
// currentText — that divergence drives delta emission).
func (s *freezeStore) freeze(key, currentText string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.snap[key]; ok {
		// Move to end of order (LRU touch).
		for i, k := range s.order {
			if k == key {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
		s.order = append(s.order, key)
		return existing
	}

	s.snap[key] = currentText
	s.order = append(s.order, key)
	for len(s.order) > s.cap {
		delete(s.snap, s.order[0])
		s.order = s.order[1:]
	}
	return currentText
}

// envMarkers are patterns that identify a Claude Code environment/context block
// — the part of the system prompt that carries cwd, platform, date, git status.
var envMarkers = []string{
	"<env>",
	"Working directory",
	"Is directory a git repo",
	"Today's date",
	"Current branch",
	"Recent commits",
	"gitStatus",
	"Platform:",
	"OS Version",
}

// looksLikeEnvBlock returns true if the text contains markers identifying it as
// a Claude Code environment/context block.
func looksLikeEnvBlock(text string) bool {
	for _, m := range envMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// hasVolatileTokens checks whether the text contains volatile tokens (dates,
// UUIDs, hex hashes, git SHAs) that change request-to-request.
func hasVolatileTokens(text string) bool {
	return len(isoDateTimeRE.FindAllString(text, -1)) > 0 ||
		len(uuidRE.FindAllString(text, -1)) > 0 ||
		len(dateRE.FindAllString(text, -1)) > 0
}

// lineageKey computes a fingerprint of the stable part of the request prefix:
// system prompt minus env-like blocks, plus tools. Same-lineage requests share
// a cache-anchor ancestry — every turn of one conversation.
func lineageKey(req *core.Request) string {
	var stableSystem strings.Builder

	for _, msg := range req.Messages {
		if msg.Role == core.RoleSystem {
			if looksLikeEnvBlock(msg.Content) {
				continue // Skip volatile env blocks.
			}
			stableSystem.WriteString(msg.Content)
			stableSystem.WriteByte('\n')
		}
	}

	// Serialize tools for fingerprinting.
	payload := map[string]interface{}{
		"system": stableSystem.String(),
		"tools":  req.Tools,
	}
	raw, _ := json.Marshal(payload)
	h := sha256.Sum256(raw)
	return fmt.Sprintf("%x", h[:8])
}

// computeDelta returns the lines in currentText that differ from frozenText.
// Lines that are blank or only whitespace are excluded from the delta.
func computeDelta(frozenText, currentText string) []string {
	frozenLines := make(map[string]struct{}, 64)
	for _, ln := range strings.Split(frozenText, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			frozenLines[ln] = struct{}{}
		}
	}

	var delta []string
	for _, ln := range strings.Split(currentText, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if _, seen := frozenLines[ln]; !seen {
			delta = append(delta, ln)
		}
	}
	return delta
}

// envUpdateBlock wraps changed env lines in a tagged block that tells the model
// what values have changed since the frozen snapshot.
func envUpdateBlock(delta []string) string {
	if len(delta) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"<env-update>\nThese environment values changed since the session start; use them as current:\n%s\n</env-update>",
		strings.Join(delta, "\n"),
	)
}

// findEnvBlockIndex returns the index of the system message containing the env
// block, and its content text. Returns -1 if not found.
func findEnvBlock(req *core.Request) (int, string) {
	for i, msg := range req.Messages {
		if msg.Role == core.RoleSystem && looksLikeEnvBlock(msg.Content) && hasVolatileTokens(msg.Content) {
			return i, msg.Content
		}
	}
	return -1, ""
}

// appendToLastUserMessage appends a text block to the content of the last user
// message in the request. Returns true if successful.
func appendToLastUserMessage(req *core.Request, text string) bool {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == core.RoleUser {
			req.Messages[i].Content += "\n" + text
			return true
		}
	}
	return false
}

// sortTools sorts req.Tools by (name, canonical JSON of parameters). Returns
// true if the order changed. Sorting tools makes the output deterministic
// regardless of MCP server startup timing or tool-search deferral toggling.
func sortTools(req *core.Request) bool {
	if len(req.Tools) < 2 {
		return false
	}

	type toolKey struct {
		name      string
		canonical string
	}

	makeKey := func(t core.Tool) toolKey {
		raw, _ := json.Marshal(t)
		return toolKey{
			name:      t.Function.Name,
			canonical: string(raw),
		}
	}

	before := make([]toolKey, len(req.Tools))
	for i, t := range req.Tools {
		before[i] = makeKey(t)
	}

	sort.SliceStable(req.Tools, func(i, j int) bool {
		ki, kj := makeKey(req.Tools[i]), makeKey(req.Tools[j])
		if ki.name == kj.name {
			return ki.canonical < kj.canonical
		}
		return ki.name < kj.name
	})

	for i, t := range req.Tools {
		if makeKey(t) != before[i] {
			return true
		}
	}
	return false
}
