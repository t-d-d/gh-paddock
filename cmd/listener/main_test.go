package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"

	"github.com/actions/scaleset"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/google/uuid"

	"github.com/t-d-d/gh-paddock/internal/github"
	"github.com/t-d-d/gh-paddock/internal/heartbeat"
	"github.com/t-d-d/gh-paddock/internal/runners"
	"github.com/t-d-d/gh-paddock/internal/serve"
)

func envWith(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// syncBuffer is a log both the listener's goroutines and the test touch.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var appKey = func() string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	raw, _ := json.Marshal(map[string]any{"appId": 42, "privateKey": string(pemKey)})
	return string(raw)
}()

// fakeCloud is every client the listener calls, offline.
type fakeCloud struct {
	params   map[string]string
	paramErr error
	checkErr error
	checks   int
	repos    map[string]github.Repo
	session  *fakeSession
	runs     []*ecs.RunTaskInput
	// deletedSets are the scale sets deleted, by id.
	deletedSets []int
	mu          sync.Mutex
}

// setList replaces the repo list, or makes it unreadable, while the
// listener runs.
func (c *fakeCloud) setList(raw string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.params["/paddock/repos"] = raw
	c.paramErr = err
}

func (c *fakeCloud) Parameter(_ context.Context, name string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if name == "/paddock/repos" && c.paramErr != nil {
		return "", false, c.paramErr
	}
	v, ok := c.params[name]
	return v, ok, nil
}

func (c *fakeCloud) App(key github.AppKey) (app, error) {
	if key.AppID != 42 {
		return nil, fmt.Errorf("app id %d", key.AppID)
	}
	return c, nil
}

func (c *fakeCloud) Check(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks++
	return c.checkErr
}

func (c *fakeCloud) Installation(_ context.Context, org string) (serve.Installation, int64, error) {
	if org != "acme" {
		return nil, 0, fmt.Errorf("the App is not installed on %s", org)
	}
	return c, 1, nil
}

func (c *fakeCloud) Repo(_ context.Context, owner, name string) (github.Repo, error) {
	r, ok := c.repos[owner+"/"+name]
	if !ok {
		return github.Repo{}, errors.New("not found")
	}
	return r, nil
}

func (c *fakeCloud) EnsureRunnerGroup(context.Context, string, string, int64) error { return nil }

func (c *fakeCloud) RunnerGroups(context.Context, string) ([]string, error) {
	return []string{"paddock-widgets-dev"}, nil
}

func (c *fakeCloud) DeleteRunnerGroup(context.Context, string, string) error { return nil }

func (c *fakeCloud) DeleteRunnerScaleSet(_ context.Context, id int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletedSets = append(c.deletedSets, id)
	return nil
}

func (c *fakeCloud) ScaleSets(github.AppKey, string, int64) (serve.ScaleSets, error) { return c, nil }

func (c *fakeCloud) GetRunnerGroupByName(_ context.Context, name string) (*scaleset.RunnerGroup, error) {
	return &scaleset.RunnerGroup{ID: 3, Name: name}, nil
}

func (c *fakeCloud) GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error) {
	return nil, nil
}

func (c *fakeCloud) CreateRunnerScaleSet(_ context.Context, s *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	out := *s
	out.ID = 9
	return &out, nil
}

func (c *fakeCloud) UpdateRunnerScaleSet(_ context.Context, _ int, s *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	return s, nil
}

func (c *fakeCloud) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "jit"}, nil
}

func (c *fakeCloud) OpenSession(context.Context, int, string) (serve.Session, error) {
	return c.session, nil
}

func (c *fakeCloud) ECS() runners.ECS             { return fakeECS{c} }
func (c *fakeCloud) SSM() runners.SSM             { return fakeSSM{} }
func (c *fakeCloud) Objects() runners.Objects     { return fakeS3{} }
func (c *fakeCloud) Presigner() runners.Presigner { return fakeS3{} }
func (c *fakeCloud) Metrics() runners.Metrics     { return fakeMetrics{} }

type fakeMetrics struct{}

func (fakeMetrics) GetMetricData(context.Context, *cloudwatch.GetMetricDataInput, ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	return &cloudwatch.GetMetricDataOutput{}, nil
}

func (c *fakeCloud) GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error) {
	return nil, nil
}
func (c *fakeCloud) RemoveRunner(context.Context, int64) error { return nil }

func (c *fakeCloud) Run(context.Context, string, string, int64) (github.Run, error) {
	return github.Run{}, nil
}
func (c *fakeCloud) RunJobs(context.Context, string, string, int64, int) ([]github.Job, error) {
	return nil, nil
}
func (c *fakeCloud) RerunJob(context.Context, string, string, int64) error { return nil }

type fakeS3 struct{}

func (fakeS3) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return &s3.PutObjectOutput{}, nil
}
func (fakeS3) DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return &s3.DeleteObjectOutput{}, nil
}
func (fakeS3) PresignGetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	return &v4.PresignedHTTPRequest{URL: "https://configs.s3.example/" + aws.ToString(in.Key)}, nil
}

type fakeECS struct{ c *fakeCloud }

func (f fakeECS) ListTasks(context.Context, *ecs.ListTasksInput, ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	return &ecs.ListTasksOutput{}, nil
}
func (f fakeECS) DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	return &ecs.DescribeTasksOutput{}, nil
}
func (f fakeECS) RegisterTaskDefinition(context.Context, *ecs.RegisterTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error) {
	return &ecs.RegisterTaskDefinitionOutput{TaskDefinition: &ecstypes.TaskDefinition{TaskDefinitionArn: aws.String("def")}}, nil
}
func (f fakeECS) DeregisterTaskDefinition(context.Context, *ecs.DeregisterTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.DeregisterTaskDefinitionOutput, error) {
	return &ecs.DeregisterTaskDefinitionOutput{}, nil
}
func (f fakeECS) DeleteTaskDefinitions(context.Context, *ecs.DeleteTaskDefinitionsInput, ...func(*ecs.Options)) (*ecs.DeleteTaskDefinitionsOutput, error) {
	return &ecs.DeleteTaskDefinitionsOutput{}, nil
}
func (f fakeECS) StopTask(context.Context, *ecs.StopTaskInput, ...func(*ecs.Options)) (*ecs.StopTaskOutput, error) {
	return &ecs.StopTaskOutput{}, nil
}
func (f fakeECS) TagResource(context.Context, *ecs.TagResourceInput, ...func(*ecs.Options)) (*ecs.TagResourceOutput, error) {
	return &ecs.TagResourceOutput{}, nil
}
func (f fakeECS) RunTask(_ context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	f.c.runs = append(f.c.runs, in)
	return &ecs.RunTaskOutput{Tasks: []ecstypes.Task{{TaskArn: aws.String("arn:aws:ecs:us-east-1:123456789012:task/paddock/abc")}}}, nil
}

type fakeSSM struct{}

func (fakeSSM) PutParameter(context.Context, *ssm.PutParameterInput, ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	return &ssm.PutParameterOutput{}, nil
}
func (fakeSSM) DeleteParameter(context.Context, *ssm.DeleteParameterInput, ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	return &ssm.DeleteParameterOutput{}, nil
}
func (fakeSSM) GetParametersByPath(context.Context, *ssm.GetParametersByPathInput, ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	return &ssm.GetParametersByPathOutput{}, nil
}

type fakeSession struct {
	messages chan *scaleset.RunnerScaleSetMessage
	closed   chan struct{}
}

func (s *fakeSession) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case m := <-s.messages:
		return m, nil
	}
}
func (s *fakeSession) DeleteMessage(context.Context, int) error              { return nil }
func (s *fakeSession) AcquireJobs(context.Context, []int64) ([]int64, error) { return nil, nil }
func (s *fakeSession) Close(context.Context) error                           { close(s.closed); return nil }
func (s *fakeSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: uuid.New(), Statistics: &scaleset.RunnerScaleSetStatistic{}}
}

func connectTo(c *fakeCloud) connector {
	return func(context.Context, string) (cloud, error) { return c, nil }
}

func listenerEnv(path string) map[string]string {
	return map[string]string{
		"PADDOCK_HEARTBEAT_FILE":            path,
		"PADDOCK_IMAGE_DIGEST":              "sha256:abc123",
		"PADDOCK_CLUSTER":                   "paddock",
		"PADDOCK_RUNNER_IMAGE":              "registry.example/runner@sha256:def",
		"PADDOCK_RUNNER_EXECUTION_ROLE_ARN": "arn:aws:iam::123456789012:role/runner",
		"PADDOCK_RUNNER_LOG_GROUP":          "/paddock/runners",
		"PADDOCK_RUNNER_SECURITY_GROUP":     "sg-1",
		"PADDOCK_SUBNETS":                   "subnet-1,subnet-2",
		"PADDOCK_CONFIG_BUCKET":             "paddock-runnerconfigs-abc",
		"AWS_REGION":                        "us-east-1",
	}
}

func TestHealthcheckPassesOnAFreshBeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	if err := heartbeat.Beat(path, time.Now()); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"-healthcheck"}, envWith(map[string]string{"PADDOCK_HEARTBEAT_FILE": path}), &out, &errOut, nil)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestHealthcheckFailsWithNoBeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"-healthcheck"}, envWith(map[string]string{"PADDOCK_HEARTBEAT_FILE": path}), &out, &errOut, nil)
	if code == 0 {
		t.Fatal("the health check passed with no heartbeat file")
	}
	if !strings.Contains(errOut.String(), "no heartbeat") {
		t.Fatalf("stderr %q does not say why", errOut.String())
	}
}

// A beat may be as old as the longest poll, and the listener must still be
// counted alive; a listener with no pool checks the App often enough to
// keep its beat fresh.
func TestTheHealthCheckOutlastsTheLongestPoll(t *testing.T) {
	if beatMaxAge <= scaleset.MessagePollTimeout {
		t.Fatalf("beatMaxAge %s is no longer than a poll may take, %s", beatMaxAge, scaleset.MessagePollTimeout)
	}
	if beatMaxAge < 2*idleCheckEvery {
		t.Fatalf("beatMaxAge %s is under two idle checks of %s", beatMaxAge, idleCheckEvery)
	}
	if beatMaxAge > 5*time.Minute {
		t.Fatalf("beatMaxAge %s lets a stuck listener look healthy too long", beatMaxAge)
	}
}

func TestTheListenerWillNotStartWithoutItsSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	var out, errOut bytes.Buffer
	code := run(context.Background(), nil, envWith(map[string]string{"PADDOCK_HEARTBEAT_FILE": path}), &out, &errOut, connectTo(&fakeCloud{}))
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "PADDOCK_CLUSTER") {
		t.Fatalf("logs do not name the missing setting:\n%s", out.String())
	}
	if heartbeat.Check(path, time.Minute, time.Now()) == nil {
		t.Fatal("a listener that never started beat")
	}
}

// The App call comes before the first beat, so a bad key never turns the
// task healthy, and the deploy fails.
func TestABadAppKeyFailsBeforeAnyBeat(t *testing.T) {
	cases := map[string]*fakeCloud{
		"no key":         {params: map[string]string{}},
		"key not JSON":   {params: map[string]string{"/paddock/github-app": "nope"}},
		"GitHub refuses": {params: map[string]string{"/paddock/github-app": appKey}, checkErr: errors.New("401 Bad credentials")},
	}
	for name, c := range cases {
		path := filepath.Join(t.TempDir(), "heartbeat")
		var out, errOut bytes.Buffer
		code := run(context.Background(), nil, envWith(listenerEnv(path)), &out, &errOut, connectTo(c))
		if code != 1 {
			t.Errorf("%s: exit %d", name, code)
		}
		if heartbeat.Check(path, time.Minute, time.Now()) == nil {
			t.Errorf("%s: the listener beat", name)
		}
		if strings.Contains(out.String(), "PRIVATE KEY") {
			t.Errorf("%s: the logs carry the key", name)
		}
	}
}

func runUntilBeat(t *testing.T, c *fakeCloud) (cancel func() (int, string)) {
	t.Helper()
	stop, _ := runLive(t, c)
	return stop
}

// runLive runs the listener until its first beat, and gives its log as it
// grows.
func runLive(t *testing.T, c *fakeCloud) (cancel func() (int, string), logs *syncBuffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "heartbeat")
	ctx, stop := context.WithCancel(context.Background())
	out := &syncBuffer{}
	var errOut bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, nil, envWith(listenerEnv(path)), out, &errOut, connectTo(c)) }()
	deadline := time.Now().Add(5 * time.Second)
	for heartbeat.Check(path, time.Minute, time.Now()) != nil {
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("the listener wrote no beat; logs:\n%s", out.String())
		}
		time.Sleep(time.Millisecond)
	}
	return func() (int, string) {
		stop()
		select {
		case code := <-done:
			return code, out.String()
		case <-time.After(5 * time.Second):
			t.Fatal("the listener did not stop on cancel")
			return -1, ""
		}
	}, out
}

// With no list the listener serves nothing, but it still checks the App
// first and beats, and stops cleanly.
func TestWithAnEmptyListTheListenerChecksTheAppBeatsAndStops(t *testing.T) {
	c := &fakeCloud{params: map[string]string{"/paddock/github-app": appKey}}
	stop := runUntilBeat(t, c)
	code, logs := stop()
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if c.checks < 1 {
		t.Fatal("no App call before the beat")
	}
	for _, want := range []string{`"msg":"listener started"`, `"image":"sha256:abc123"`, `"msg":"no repo list; serving nothing"`, `"msg":"listener stopped"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
}

// The stack's listener-down alarm counts this line: a listener that is up
// but cannot reach GitHub logs none.
func TestTheListenerLogsThatItPollsOncePerIntervalWhileGitHubAnswers(t *testing.T) {
	was := pollLogEvery
	pollLogEvery = 20 * time.Millisecond
	t.Cleanup(func() { pollLogEvery = was })
	c := &fakeCloud{params: map[string]string{"/paddock/github-app": appKey}}
	stop := runUntilBeat(t, c)
	time.Sleep(200 * time.Millisecond)
	_, logs := stop()
	// One beat, at the start: the idle check beats again only after a
	// minute, so one interval logs it and the rest log nothing.
	if n := strings.Count(logs, `"msg":"listener polling"`); n != 1 {
		t.Fatalf("logged polling %d times for one beat:\n%s", n, logs)
	}
}

// A list the listener cannot read or parse at its start serves nothing, and
// logs the line the stack's alarm counts.
func TestAnUnreadableListAtTheStartServesNothingAndRaisesTheAlarm(t *testing.T) {
	for name, c := range map[string]*fakeCloud{
		"unreadable": {params: map[string]string{"/paddock/github-app": appKey}, paramErr: errors.New("AccessDenied")},
		"not JSON":   {params: map[string]string{"/paddock/github-app": appKey, "/paddock/repos": "{"}},
	} {
		stop := runUntilBeat(t, c)
		code, logs := stop()
		if code != 0 || !strings.Contains(logs, `"msg":"repo list unreadable"`) || strings.Contains(logs, `"msg":"pool ready"`) {
			t.Errorf("%s: exit %d, logs:\n%s", name, code, logs)
		}
	}
}

func waitForLog(t *testing.T, logs *syncBuffer, want string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(logs.String(), want) < n {
		if time.Now().After(deadline) {
			t.Fatalf("logs never said %s %d times:\n%s", want, n, logs.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// The listener re-reads the list while it runs: a repo added is served
// with no restart, and a list that later cannot be read keeps the last
// good one in force, raises the alarm, and deletes nothing.
func TestTheListIsReReadAndAnUnreadableOneKeepsTheLastGood(t *testing.T) {
	t.Run("fitness: a failed read of the list adds, changes and deletes nothing (served-repos.md)", func(t *testing.T) {
		was := listReadEvery
		listReadEvery = 10 * time.Millisecond
		t.Cleanup(func() { listReadEvery = was })
		session := &fakeSession{messages: make(chan *scaleset.RunnerScaleSetMessage), closed: make(chan struct{})}
		c := &fakeCloud{
			params:  map[string]string{"/paddock/github-app": appKey},
			repos:   map[string]github.Repo{"acme/widgets": {ID: 7, Private: true}},
			session: session,
		}
		stop, logs := runLive(t, c)
		c.setList(`{"repos": [{"repo": "acme/widgets", "pools": [{"name": "dev", "capacity": "on-demand"}]}]}`, nil)
		waitForLog(t, logs, `"msg":"pool ready"`, 1)

		c.setList("", errors.New("ThrottlingException"))
		waitForLog(t, logs, `"msg":"repo list unreadable"`, 3)
		c.setList("{", nil)
		waitForLog(t, logs, `"msg":"repo list unreadable"`, 6)

		// The pool still serves: a job on it starts a task.
		session.messages <- &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}
		deadline := time.Now().Add(5 * time.Second)
		for {
			c.mu.Lock()
			n := len(c.runs)
			c.mu.Unlock()
			if n == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the pool no longer serves after a failed read:\n%s", logs.String())
			}
			time.Sleep(time.Millisecond)
		}
		code, out := stop()
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if strings.Contains(out, "pool removed") || len(c.deletedSets) != 0 {
			t.Fatalf("a failed read removed a pool or deleted %v:\n%s", c.deletedSets, out)
		}
		if n := strings.Count(out, `"msg":"pool ready"`); n != 1 {
			t.Fatalf("the pool was set up %d times", n)
		}
	})
}

// End to end, offline: a listed repo's pool gets a scale set, its session's
// message of one assigned job starts one runner task on the pool's
// capacity, and SIGTERM closes the session.
func TestAJobOnAListedPoolStartsARunnerTask(t *testing.T) {
	session := &fakeSession{messages: make(chan *scaleset.RunnerScaleSetMessage), closed: make(chan struct{})}
	c := &fakeCloud{
		params: map[string]string{
			"/paddock/github-app": appKey,
			"/paddock/repos":      `{"repos": [{"repo": "acme/widgets", "pools": [{"name": "dev", "capacity": "on-demand"}]}]}`,
		},
		repos:   map[string]github.Repo{"acme/widgets": {ID: 7, Private: true}},
		session: session,
	}
	stop := runUntilBeat(t, c)
	session.messages <- &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.runs)
		c.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no runner task started")
		}
		time.Sleep(time.Millisecond)
	}
	code, logs := stop()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, logs)
	}
	select {
	case <-session.closed:
	default:
		t.Fatal("the session was not closed on stop")
	}
	if got := aws.ToString(c.runs[0].CapacityProviderStrategy[0].CapacityProvider); got != "FARGATE" {
		t.Fatalf("capacity %s", got)
	}
	if !strings.Contains(logs, `"msg":"pool ready"`) || !strings.Contains(logs, `"scaleSet":"paddock-widgets-dev"`) {
		t.Fatalf("logs:\n%s", logs)
	}
}

// SIGTERM is how ECS stops the listener, Fargate Spot included: it must exit
// 0, not be killed mid-write two minutes later.
func TestTheListenerExitsCleanlyOnSIGTERM(t *testing.T) {
	if os.Getenv("PADDOCK_LISTENER_SUBPROCESS") == "1" {
		// The listener's own flags, not the test binary's.
		os.Args = os.Args[:1]
		os.Exit(mainWith(connectTo(&fakeCloud{params: map[string]string{"/paddock/github-app": appKey}})))
	}
	path := filepath.Join(t.TempDir(), "heartbeat")
	cmd := exec.Command(os.Args[0], "-test.run=^TestTheListenerExitsCleanlyOnSIGTERM$")
	cmd.Env = append(os.Environ(), "PADDOCK_LISTENER_SUBPROCESS=1")
	for k, v := range listenerEnv(path) {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
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

func TestSubnetsAreReadWithoutSpacesOrEmptyEntries(t *testing.T) {
	env := listenerEnv("/tmp/unused")
	env["PADDOCK_SUBNETS"] = "subnet-1, subnet-2,"
	cfg, err := readConfig(envWith(env))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.launch.Subnets, "|"); got != "subnet-1|subnet-2" {
		t.Fatalf("subnets %q", got)
	}
	env["PADDOCK_SUBNETS"] = ","
	if _, err := readConfig(envWith(env)); err == nil || !strings.Contains(err.Error(), "PADDOCK_SUBNETS") {
		t.Fatalf("a list of no subnets was taken: %v", err)
	}
}

// The bucket takes each running task's later configs; with none, no task
// could be handed a second job, so the listener does not start.
func TestTheListenerReadsTheRunnerConfigBucket(t *testing.T) {
	env := listenerEnv("/tmp/unused")
	cfg, err := readConfig(envWith(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.launch.Bucket != "paddock-runnerconfigs-abc" {
		t.Fatalf("bucket %q", cfg.launch.Bucket)
	}
	delete(env, "PADDOCK_CONFIG_BUCKET")
	if _, err := readConfig(envWith(env)); err == nil || !strings.Contains(err.Error(), "PADDOCK_CONFIG_BUCKET") {
		t.Fatalf("started with no bucket: %v", err)
	}
}
