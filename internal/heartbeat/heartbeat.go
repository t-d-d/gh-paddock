// Package heartbeat is how the listener tells ECS it is alive. The listener
// writes the time to a file on each beat, and its container health check
// (`listener -healthcheck`) reads it back. Once the listener polls GitHub, a
// beat follows each successful poll, so a healthy task is one that is
// talking to GitHub, not merely one whose process is up.
package heartbeat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Beat records now as the latest beat. It writes a temporary file and
// renames it over the old one, so a check never reads half a beat.
func Beat(path string, now time.Time) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".heartbeat-*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(now.UTC().Format(time.RFC3339Nano)); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Check fails unless the latest beat is no older than maxAge at now. A beat
// from the future fails too: it proves nothing about the listener.
func Check(path string, maxAge time.Duration, now time.Time) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no heartbeat: %w", err)
	}
	beat, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("heartbeat holds no time: %w", err)
	}
	age := now.Sub(beat)
	if age < -5*time.Second {
		return fmt.Errorf("heartbeat is %s in the future", -age)
	}
	if age > maxAge {
		return fmt.Errorf("heartbeat is %s old, over %s", age.Round(time.Second), maxAge)
	}
	return nil
}

// Run beats at once and then every interval until ctx is cancelled, when it
// returns nil. A failed write ends it with the error.
func Run(ctx context.Context, path string, interval time.Duration, clock func() time.Time) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := Beat(path, clock()); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
