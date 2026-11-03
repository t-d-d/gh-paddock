package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/t-d-d/gh-paddock/internal/heartbeat"
)

func envWith(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestHealthcheckPassesOnAFreshBeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	if err := heartbeat.Beat(path, time.Now()); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"-healthcheck"}, envWith(map[string]string{"PADDOCK_HEARTBEAT_FILE": path}), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestHealthcheckFailsWithNoBeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"-healthcheck"}, envWith(map[string]string{"PADDOCK_HEARTBEAT_FILE": path}), &out, &errOut)
	if code == 0 {
		t.Fatal("the health check passed with no heartbeat file")
	}
	if !strings.Contains(errOut.String(), "no heartbeat") {
		t.Fatalf("stderr %q does not say why", errOut.String())
	}
}

// The listener runs until SIGTERM, which reaches run as a cancelled context,
// and logs its start and stop as JSON lines.
func TestListenerBeatsAndStopsOnCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, nil, envWith(map[string]string{"PADDOCK_HEARTBEAT_FILE": path, "PADDOCK_IMAGE_DIGEST": "sha256:abc123"}), &out, &errOut)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for heartbeat.Check(path, time.Minute, time.Now()) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the listener wrote no beat")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d on cancel, stderr %q", code, errOut.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the listener did not stop on cancel")
	}
	logs := out.String()
	for _, want := range []string{`"msg":"listener started"`, `"image":"sha256:abc123"`, `"msg":"listener stopped"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
}
