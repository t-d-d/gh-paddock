// Command listener is gh-paddock's listener: the one long-running task that
// polls GitHub's runner scale-set API and starts runner tasks to match.
//
// On start it reads the App key and checks it with GitHub before its first
// heartbeat, so a bad key fails the deploy. It reads the repo list, makes
// each pool's runner group and scale set, and runs one message session per
// pool. A heartbeat follows each time GitHub answers. On SIGTERM it closes
// its sessions and exits 0.
//
//	listener               run until SIGTERM
//	listener -healthcheck  exit 0 if the last beat is fresh: the
//	                       container health check ECS runs
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/actions/scaleset"

	"github.com/t-d-d/gh-paddock/internal/github"
	"github.com/t-d-d/gh-paddock/internal/heartbeat"
	"github.com/t-d-d/gh-paddock/internal/repos"
	"github.com/t-d-d/gh-paddock/internal/runners"
	"github.com/t-d-d/gh-paddock/internal/serve"
)

const (
	defaultHeartbeatFile = "/tmp/heartbeat"
	defaultAppKeyParam   = "/paddock/github-app"
	defaultReposParam    = "/paddock/repos"
	// With no pool to poll, the listener checks the App with GitHub this
	// often, so its beat still means GitHub answers.
	idleCheckEvery = time.Minute
	// A poll holds up to about 50 seconds, and the scale-set client gives
	// one up at MessagePollTimeout. The beat may be that old, and a minute
	// more, before the listener counts as stopped.
	beatMaxAge = scaleset.MessagePollTimeout + time.Minute
)

func main() {
	os.Exit(mainWith(connectAWS))
}

func mainWith(connect connector) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	return run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, connect)
}

// config is the listener's settings, from its environment, which the stack
// sets.
type config struct {
	heartbeatFile string
	appKeyParam   string
	reposParam    string
	launch        runners.Launch
}

func readConfig(getenv func(string) string) (config, error) {
	or := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	c := config{
		heartbeatFile: or("PADDOCK_HEARTBEAT_FILE", defaultHeartbeatFile),
		appKeyParam:   or("PADDOCK_APP_KEY_PARAMETER", defaultAppKeyParam),
		reposParam:    or("PADDOCK_REPOS_PARAMETER", defaultReposParam),
		launch: runners.Launch{
			Cluster:          getenv("PADDOCK_CLUSTER"),
			Image:            getenv("PADDOCK_RUNNER_IMAGE"),
			ExecutionRoleARN: getenv("PADDOCK_RUNNER_EXECUTION_ROLE_ARN"),
			LogGroup:         getenv("PADDOCK_RUNNER_LOG_GROUP"),
			Region:           getenv("AWS_REGION"),
			SecurityGroup:    getenv("PADDOCK_RUNNER_SECURITY_GROUP"),
			Bucket:           getenv("PADDOCK_CONFIG_BUCKET"),
		},
	}
	for _, subnet := range strings.Split(getenv("PADDOCK_SUBNETS"), ",") {
		if subnet = strings.TrimSpace(subnet); subnet != "" {
			c.launch.Subnets = append(c.launch.Subnets, subnet)
		}
	}
	var missing []string
	for key, v := range map[string]string{
		"PADDOCK_CLUSTER":                   c.launch.Cluster,
		"PADDOCK_RUNNER_IMAGE":              c.launch.Image,
		"PADDOCK_RUNNER_EXECUTION_ROLE_ARN": c.launch.ExecutionRoleARN,
		"PADDOCK_RUNNER_LOG_GROUP":          c.launch.LogGroup,
		"PADDOCK_RUNNER_SECURITY_GROUP":     c.launch.SecurityGroup,
		"PADDOCK_SUBNETS":                   strings.Join(c.launch.Subnets, ","),
		"PADDOCK_CONFIG_BUCKET":             c.launch.Bucket,
		"AWS_REGION":                        c.launch.Region,
	} {
		if v == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("the environment lacks %s", strings.Join(slices.Sorted(slices.Values(missing)), ", "))
	}
	return c, nil
}

// app is the GitHub App as the listener uses it.
type app interface {
	Check(ctx context.Context) error
	serve.App
}

// cloud is every client the listener reaches the world through. The tests
// fake it at this boundary.
type cloud interface {
	// Parameter reads a parameter, decrypted. found is false when it does
	// not exist.
	Parameter(ctx context.Context, name string) (value string, found bool, err error)
	App(key github.AppKey) (app, error)
	ScaleSets(key github.AppKey, org string, installationID int64) (serve.ScaleSets, error)
	ECS() runners.ECS
	SSM() runners.SSM
	Objects() runners.Objects
	Presigner() runners.Presigner
}

type connector func(ctx context.Context, region string) (cloud, error)

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, connect connector) int {
	flags := flag.NewFlagSet("listener", flag.ContinueOnError)
	flags.SetOutput(stderr)
	healthcheck := flags.Bool("healthcheck", false, "exit 0 if the last heartbeat is fresh")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if *healthcheck {
		path := getenv("PADDOCK_HEARTBEAT_FILE")
		if path == "" {
			path = defaultHeartbeatFile
		}
		if err := heartbeat.Check(path, beatMaxAge, time.Now()); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}

	log := slog.New(slog.NewJSONHandler(stdout, nil))
	log.Info("listener started", "image", getenv("PADDOCK_IMAGE_DIGEST"))
	cfg, err := readConfig(getenv)
	if err != nil {
		log.Error("listener cannot start", "error", err.Error())
		return 1
	}
	if err := listen(ctx, cfg, connect, log); err != nil {
		log.Error("listener cannot start", "error", err.Error())
		return 1
	}
	log.Info("listener stopped")
	return 0
}

func listen(ctx context.Context, cfg config, connect connector, log *slog.Logger) error {
	c, err := connect(ctx, cfg.launch.Region)
	if err != nil {
		return err
	}
	raw, found, err := c.Parameter(ctx, cfg.appKeyParam)
	if err != nil {
		return fmt.Errorf("read the App key: %w", err)
	}
	if !found {
		return fmt.Errorf("the App key's parameter %s does not exist", cfg.appKeyParam)
	}
	key, err := github.ParseAppKey(raw)
	if err != nil {
		return err
	}
	gh, err := c.App(key)
	if err != nil {
		return err
	}
	if err := gh.Check(ctx); err != nil {
		return fmt.Errorf("GitHub refused the App key: %w", err)
	}
	beat := func() {
		if err := heartbeat.Beat(cfg.heartbeatFile, time.Now()); err != nil {
			log.Error("heartbeat failed", "error", err.Error())
		}
	}
	beat()
	log.Info("GitHub accepted the App key")

	list := readList(ctx, c, cfg.reposParam, log)
	pools := serve.Setup(ctx, gh, func(org string, id int64) (serve.ScaleSets, error) {
		return c.ScaleSets(key, org, id)
	}, list, log)
	log.Info("serving", "pools", len(pools))

	owner, _ := os.Hostname()
	var wg sync.WaitGroup
	for _, p := range pools {
		scaler := &runners.Scaler{
			ECS: c.ECS(), SSM: c.SSM(), JIT: p.ScaleSets, Objects: c.Objects(), Presigner: c.Presigner(),
			Launch: cfg.launch,
			Repo:   p.Repo, Pool: p.Pool, ScaleSetID: p.ScaleSetID,
			Log: log.With("repo", p.Repo.FullName(), "pool", p.Pool.Name),
		}
		open := func(ctx context.Context) (serve.Session, error) {
			return p.ScaleSets.OpenSession(ctx, p.ScaleSetID, owner)
		}
		wg.Go(func() { serve.Run(ctx, p, open, scaler, beat, log, serve.Backoff) })
	}
	if len(pools) == 0 {
		wg.Go(func() { idle(ctx, gh, beat, log) })
	}
	wg.Wait()
	return nil
}

// readList reads the repo list. A missing list serves nothing; so does one
// that cannot be read or parsed, which is logged.
func readList(ctx context.Context, c cloud, name string, log *slog.Logger) repos.List {
	raw, found, err := c.Parameter(ctx, name)
	if err != nil {
		log.Error("could not read the repo list; serving nothing", "parameter", name, "error", err.Error())
		return repos.List{}
	}
	if !found {
		log.Info("no repo list; serving nothing", "parameter", name)
		return repos.List{}
	}
	list, err := repos.Parse(raw)
	if err != nil {
		log.Error("could not parse the repo list; serving nothing", "parameter", name, "error", err.Error())
		return repos.List{}
	}
	return list
}

// idle checks the App with GitHub each minute while no pool is served, so
// the beat keeps meaning that GitHub answers.
func idle(ctx context.Context, gh app, beat func(), log *slog.Logger) {
	ticker := time.NewTicker(idleCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := gh.Check(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Error("GitHub did not answer the App's check", "error", err.Error())
			}
			continue
		}
		beat()
	}
}
