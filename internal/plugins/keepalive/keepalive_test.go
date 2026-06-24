package keepalive

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

func TestSessionKey(t *testing.T) {
	req1 := &core.Request{Model: "deepseek-v4-flash"}
	req2 := &core.Request{Model: "deepseek-v4-flash"}
	req3 := &core.Request{Model: "claude-sonnet-4-6"}

	if sessionKey(req1) != sessionKey(req2) {
		t.Error("same model should produce same session key")
	}
	if sessionKey(req1) == sessionKey(req3) {
		t.Error("different models should produce different session keys")
	}
}

func TestIsDue(t *testing.T) {
	k := &Keepalive{
		interval: 300 * time.Second,
		idleStop: 36000 * time.Second,
	}

	now := time.Now()

	// Not idle long enough.
	slot := &keepaliveSlot{lastReal: now}
	if k.isDue(slot, now) {
		t.Error("fresh slot should not be due")
	}

	// Idle enough but never fired.
	slot.lastReal = now.Add(-400 * time.Second)
	if !k.isDue(slot, now) {
		t.Error("idle slot should be due")
	}

	// Idle too long (past idleStop).
	slot.lastReal = now.Add(-40000 * time.Second)
	if k.isDue(slot, now) {
		t.Error("overdue slot should not be due")
	}

	// Recently fired.
	slot.lastReal = now.Add(-400 * time.Second)
	slot.lastFire = now.Add(-50 * time.Second)
	if k.isDue(slot, now) {
		t.Error("recently-fired slot should not be due")
	}
}

func TestIsAnthropicBody(t *testing.T) {
	// OpenAI format body.
	openaiBody, _ := json.Marshal(map[string]interface{}{
		"model": "gpt-4",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "hello"},
		},
	})
	if isAnthropicBody(openaiBody) {
		t.Error("OpenAI body should not be detected as Anthropic")
	}

	// Anthropic format body.
	anthropicBody, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 100,
		"system":     "You are a helpful assistant",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "hello"},
		},
	})
	if !isAnthropicBody(anthropicBody) {
		t.Error("Anthropic body should be detected")
	}

	// Anthropic with block system.
	anthropicBlockBody, _ := json.Marshal(map[string]interface{}{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 100,
		"system": []map[string]interface{}{
			{"type": "text", "text": "You are helpful"},
		},
		"messages": []map[string]interface{}{
			{"role": "user", "content": "hello"},
		},
	})
	if !isAnthropicBody(anthropicBlockBody) {
		t.Error("Anthropic body with block system should be detected")
	}
}

func TestKeepaliveInit(t *testing.T) {
	k := &Keepalive{}
	if err := k.Init(map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if k.interval != 1800*time.Second {
		t.Errorf("default interval should be 1800s, got %v", k.interval)
	}
	if k.idleStop != 36000*time.Second {
		t.Errorf("default idleStop should be 36000s, got %v", k.idleStop)
	}
	if k.slotCap != 8 {
		t.Errorf("default slotCap should be 8, got %d", k.slotCap)
	}
	if k.httpClient == nil {
		t.Error("httpClient should be created")
	}

	// Clean up the background goroutine.
	close(k.tickerDone)
}

func TestKeepaliveCustomConfig(t *testing.T) {
	k := &Keepalive{}
	if err := k.Init(map[string]interface{}{
		"interval_s":   600.0,
		"idle_stop_s":  7200.0,
		"slot_cap":     4.0,
		"gateway_url":  "http://127.0.0.1:9999",
	}); err != nil {
		t.Fatal(err)
	}
	if k.interval != 600*time.Second {
		t.Errorf("interval should be 600s, got %v", k.interval)
	}
	if k.idleStop != 7200*time.Second {
		t.Errorf("idleStop should be 7200s, got %v", k.idleStop)
	}
	if k.slotCap != 4 {
		t.Errorf("slotCap should be 4, got %d", k.slotCap)
	}
	if k.gatewayURL != "http://127.0.0.1:9999" {
		t.Errorf("gatewayURL should be set, got %s", k.gatewayURL)
	}
	close(k.tickerDone)
}
