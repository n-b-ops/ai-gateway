# Patches — n-b-ops/ai-gateway fork

Fork: `github.com/n-b-ops/ai-gateway` (origin)
Upstream: `github.com/ferro-labs/ai-gateway`
Branch strategy: `main` tracks upstream cleanly; all patches live on `ops-llm`.
Merge: `git fetch upstream && git checkout main && git merge upstream/main && git checkout ops-llm && git merge main`

## Patch List (7 commits on ops-llm)

### 1. `8df17fb` — add zai provider: Anthropic-compatible provider for api.z.ai

**Files**: `providers/zai/` (new provider), `providers/names.go`, `providers/providers_list.go`

Adds a new provider for z.ai's Anthropic-compatible API endpoint (`api.z.ai/api/anthropic`).
Includes full test suite and provider registration.

**Why**: Enables routing through z.ai for testing Claude models via z.ai's Anthropic-compatible API.
**Risk**: Low — new provider, doesn't touch existing code.

---

### 2. `c9fed31` — zai: strip routing prefix from model name before forwarding to z.ai

**Files**: `providers/zai/zai.go`

Adds `resolveModel()` to strip `zai/` prefix from model names before forwarding.
Makes z.ai work with the routing prefix system used by other providers.

**Why**: Consistent with ds/, or/ prefix routing used by other providers.
**Risk**: Low — single-provider change.

---

### 3. `39741d2` — zai: remove claude- prefix from SupportsModel

**Files**: `providers/zai/zai.go`

Removes `claude-` prefix from `SupportsModel()` to avoid provider conflict with the Anthropic provider.
Without this, requests with `claude-sonnet-*` model names would match z.ai instead of Anthropic.

**Why**: Provider disambiguation — Anthropic provider should handle Claude models, not z.ai.
**Risk**: Low — single-function change.

---

### 4. `dfee248` — restore permafrost provider, add ds-/or- prefix stripping

**Files**: `providers/permafrost/`, `providers/deepseek/deepseek.go`, `providers/openrouter/openrouter.go`, `providers/names.go`, `providers/providers_list.go`

Restores the Permafrost provider (which was apparently removed upstream) and adds `ds/` and `or/` prefix stripping to the DeepSeek and OpenRouter providers.

**Why**: Permafrost is essential for DeepSeek cache alignment. Prefix stripping enables agent-specific routing.
**Risk**: Medium — touches multiple providers and provider registration.

---

### 5. `aa2b404` — permafrost: add ds- prefix stripping

**Files**: `providers/permafrost/permafrost.go`

Changes `SupportsModel()` to `return true` (from prefix matching on `deepseek-`) and adds `resolveModel()` that strips `ds/` prefix. Makes Permafrost the universal fallback provider.

**Why**: Permafrost is a transparent passthrough — it shouldn't filter model names. Without this, non-`deepseek-*` models bypass Permafrost and miss cache alignment.
**Risk**: Low — single-provider change, but important for routing correctness.

---

### 6. `ea98963` — openrouter: call resolveModel in Complete and CompleteStream

**Files**: `providers/openrouter/openrouter.go`

Adds `resolveModel()` calls in both `Complete()` and `CompleteStream()` to strip the `or/` prefix before forwarding to OpenRouter. Without this, OpenRouter receives `or/deepseek-v4-flash` which it doesn't recognize.

**Why**: Bug fix — prefix stripping was missing from the actual provider call methods.
**Risk**: Medium — if the prefix isn't stripped, upstream receives unknown model names.

---

### 7. `e11038c` — prefixes: change separator from - to / for ds/, zai/, or/

**Files**: `providers/deepseek/deepseek.go`, `providers/openrouter/openrouter.go`, `providers/permafrost/permafrost.go`, `providers/zai/zai.go`

Changes routing prefix separator from `-` (e.g., `ds-deepseek-chat`) to `/` (e.g., `ds/deepseek-chat`). The `/` separator aligns with how model name paths typically work and is easier to parse.

**Why**: Cleaner routing syntax — `ds/` reads as "provider/model" which is more intuitive.
**Risk**: Low — configuration change, requires config and client model names to use `ds/model` format.
