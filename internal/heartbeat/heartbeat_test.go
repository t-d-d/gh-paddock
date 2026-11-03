package heartbeat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)

func TestCheckPassesAFreshBeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	if err := Beat(path, t0); err != nil {
		t.Fatal(err)
	}
	if err := Check(path, time.Minute, t0.Add(59*time.Second)); err != nil {
		t.Fatalf("a beat 59s old failed a one-minute check: %v", err)
	}
}

func TestCheckFailsAStaleBeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	if err := Beat(path, t0); err != nil {
		t.Fatal(err)
	}
	if err := Check(path, time.Minute, t0.Add(61*time.Second)); err == nil {
		t.Fatal("a beat 61s old passed a one-minute check")
	}
}

func TestCheckFailsWithNoBeat(t *testing.T) {
	if err := Check(filepath.Join(t.TempDir(), "heartbeat"), time.Minute, t0); err == nil {
		t.Fatal("a missing heartbeat file passed")
	}
}

func TestCheckFailsOnAnUnreadableBeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	if err := os.WriteFile(path, []byte("not a time"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Check(path, time.Minute, t0); err == nil {
		t.Fatal("a heartbeat file holding no time passed")
	}
}

// A beat from the future means a clock went backwards or the file was
// written by hand; either way it proves nothing about the listener.
func TestCheckFailsABeatFromTheFuture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	if err := Beat(path, t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := Check(path, time.Minute, t0); err == nil {
		t.Fatal("a beat ten minutes ahead passed")
	}
}

// Run beats at once and then on every tick, so the beat keeps moving.
func TestRunBeatsUntilCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, 5*time.Millisecond, time.Now) }()
	seen := map[string]bool{}
	deadline := time.Now().Add(2 * time.Second)
	for len(seen) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("Run wrote %d distinct beats in 2s, want 3", len(seen))
		}
		if raw, err := os.ReadFile(path); err == nil {
			seen[string(raw)] = true
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
