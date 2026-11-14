package heartbeat

import (
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
