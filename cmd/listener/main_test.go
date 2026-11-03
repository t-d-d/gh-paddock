package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

// One slow write must not fail the health check, and the check must still
// notice a listener that has stopped beating within a few beats.
func TestTheHealthCheckToleratesAMissedBeat(t *testing.T) {
	if beatMaxAge < 2*beatEvery {
		t.Fatalf("beatMaxAge %s is under two beats of %s", beatMaxAge, beatEvery)
	}
	if beatMaxAge > 6*beatEvery {
		t.Fatalf("beatMaxAge %s lets six beats of %s go missing", beatMaxAge, beatEvery)
	}
}

// SIGTERM is how ECS stops the listener, Fargate Spot included: it must exit
// 0, not be killed mid-write two minutes later.
func TestTheListenerExitsCleanlyOnSIGTERM(t *testing.T) {
	if os.Getenv("PADDOCK_LISTENER_SUBPROCESS") == "1" {
		// The listener's own flags, not the test binary's.
		os.Args = os.Args[:1]
		main()
		return
	}
	path := filepath.Join(t.TempDir(), "heartbeat")
	cmd := exec.Command(os.Args[0], "-test.run=^TestTheListenerExitsCleanlyOnSIGTERM$")
	cmd.Env = append(os.Environ(), "PADDOCK_LISTENER_SUBPROCESS=1", "PADDOCK_HEARTBEAT_FILE="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for heartbeat.Check(path, time.Minute, time.Now()) != nil {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("the listener wrote no beat")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the listener exited with %v on SIGTERM", err)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the listener did not exit on SIGTERM")
	}
}
