package coalesce

import (
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

func TestAnchorFingerprint(t *testing.T) {
	req := &core.Request{
		Model: "deepseek-v4-flash",
		Tools: []core.Tool{
			{Type: "function", Function: core.Function{Name: "get_weather"}},
		},
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: "You are helpful."},
			{Role: core.RoleUser, Content: "Hello"},
		},
	}

	fp1 := AnchorFingerprint(req)
	if fp1 == "" {
		t.Fatal("fingerprint should not be empty")
	}

	// Same request = same fingerprint.
	fp2 := AnchorFingerprint(req)
	if fp1 != fp2 {
		t.Error("same request should produce same fingerprint")
	}

	// Different tools = different fingerprint.
	req.Tools = append(req.Tools, core.Tool{Type: "function", Function: core.Function{Name: "extra"}})
	fp3 := AnchorFingerprint(req)
	if fp3 == fp1 {
		t.Error("different tools should produce different fingerprint")
	}
}

func TestCoalescerAcquirePass(t *testing.T) {
	c := New(Config{Enabled: true, Cap: 10, TimeoutS: 30})

	// First request: leader.
	status, gate := c.Acquire("fp1")
	if status != StatusLeader {
		t.Errorf("first acquire should be leader, got %v", status)
	}
	if gate != nil {
		t.Error("leader should not receive a gate")
	}

	// Mark warm.
	c.Release("fp1")

	// Second request: pass (warm).
	status, gate = c.Acquire("fp1")
	if status != StatusPass {
		t.Errorf("warm anchor should pass, got %v", status)
	}
	if gate != nil {
		t.Error("pass should not receive a gate")
	}
}

func TestCoalescerFollower(t *testing.T) {
	c := New(Config{Enabled: true, Cap: 10, TimeoutS: 30})

	// First request: leader.
	status, _ := c.Acquire("fp1")
	if status != StatusLeader {
		t.Fatalf("first should be leader, got %v", status)
	}

	// Second request: follower (leader still in flight).
	status, gate := c.Acquire("fp1")
	if status != StatusFollower {
		t.Fatalf("second should be follower, got %v", status)
	}
	if gate == nil {
		t.Fatal("follower should receive a gate")
	}

	// Release from a goroutine.
	done := make(chan bool)
	go func() {
		time.Sleep(10 * time.Millisecond)
		c.Release("fp1")
		done <- true
	}()

	// Follower waits — should succeed when leader releases.
	if !gate.Wait(2 * time.Second) {
		t.Error("follower should have proceeded after leader release")
	}
	<-done
}

func TestCoalescerFollowerTimeout(t *testing.T) {
	c := New(Config{Enabled: true, Cap: 10, TimeoutS: 1})

	// Leader.
	c.Acquire("fp1")

	// Follower.
	_, gate := c.Acquire("fp1")
	if gate == nil {
		t.Fatal("expected gate")
	}

	// Leader never releases — follower should timeout.
	if gate.Wait(50 * time.Millisecond) {
		t.Error("follower should timeout when leader never releases")
	}
}

func TestCoalescerFail(t *testing.T) {
	c := New(Config{Enabled: true, Cap: 10})

	c.Acquire("fp1")
	c.Fail("fp1")

	// After fail, a new acquire should be a fresh leader.
	status, _ := c.Acquire("fp1")
	if status != StatusLeader {
		t.Errorf("after fail should be fresh leader, got %v", status)
	}
}

func TestCoalescerLRU(t *testing.T) {
	c := New(Config{Enabled: true, Cap: 3, TimeoutS: 30})

	// Fill to capacity.
	c.Acquire("fp1")
	c.Acquire("fp2")
	c.Acquire("fp3")

	// Release all to mark them warm.
	c.Release("fp1")
	c.Release("fp2")
	c.Release("fp3")

	// Add a fourth — fp1 should be evicted (oldest).
	c.Acquire("fp4")

	// fp1 should be gone (evicted).
	status, _ := c.Acquire("fp1")
	if status != StatusLeader {
		t.Errorf("evicted fp1 should be a fresh leader, got %v (it was evicted)", status)
	}
}

func TestCoalescerStats(t *testing.T) {
	c := New(Config{Enabled: true, Cap: 10})
	c.Acquire("fp1")
	c.Acquire("fp2")
	c.Release("fp1")

	stats := c.Stats()
	if stats["total"].(int) != 2 {
		t.Errorf("expected 2 total, got %v", stats["total"])
	}
	if stats["warm"].(int) != 1 {
		t.Errorf("expected 1 warm, got %v", stats["warm"])
	}
	if stats["pending"].(int) != 1 {
		t.Errorf("expected 1 pending, got %v", stats["pending"])
	}
}
