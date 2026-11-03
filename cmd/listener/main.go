// Command listener is gh-paddock's listener: the one long-running task that
// polls GitHub's runner scale-set API and starts runner tasks to match.
//
// For now it is the walking skeleton. It starts, beats, and stops cleanly on
// SIGTERM, so the deploy can prove an image runs and roll back one that
// does not, before the scale-set work lands (see the v1 PRD).
//
//	listener               run until SIGTERM
//	listener -healthcheck  exit 0 if the last beat is fresh: the
//	                       container health check ECS runs
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/t-d-d/gh-paddock/internal/heartbeat"
)

const (
	defaultHeartbeatFile = "/tmp/heartbeat"
	beatEvery            = 10 * time.Second
	// Three missed beats fail the check, so one slow write does not.
	beatMaxAge = 3 * beatEvery
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("listener", flag.ContinueOnError)
	flags.SetOutput(stderr)
	healthcheck := flags.Bool("healthcheck", false, "exit 0 if the last heartbeat is fresh")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	path := getenv("PADDOCK_HEARTBEAT_FILE")
	if path == "" {
		path = defaultHeartbeatFile
	}

	if *healthcheck {
		if err := heartbeat.Check(path, beatMaxAge, time.Now()); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}

	log := slog.New(slog.NewJSONHandler(stdout, nil))
	log.Info("listener started", "image", getenv("PADDOCK_IMAGE_DIGEST"))
	if err := heartbeat.Run(ctx, path, beatEvery, time.Now); err != nil {
		log.Error("heartbeat failed", "error", err.Error())
		return 1
	}
	log.Info("listener stopped")
	return 0
}
