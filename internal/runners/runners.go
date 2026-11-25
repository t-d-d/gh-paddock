// Package runners starts a pool's runner tasks, on the pool's capacity and
// size, to match the jobs GitHub has assigned to the pool's scale set. A
// task runs one runner for one job at a time, and may run several in turn.
//
// A task's first just-in-time config goes in its own SecureString
// parameter, and the task's definition, registered for that one task, names
// that parameter alone. ECS injects it as the task starts; the task holds
// no role, so it can read nothing else. Beside the config, the parameter
// holds a presigned URL to the one object that takes the task's later
// configs (docs/constraints/runner-isolation.md).
//
// When a task's runner finishes its job, the task waits up to 30 seconds
// for a new config, with no runner registered, and exits by itself if none
// comes. The scaler hands a waiting task a config before it starts any new
// task, and only for a job already assigned to the pool
// (docs/constraints/pool-per-repo.md, remove-then-terminate.md).
//
// The scaler also recovers what fails: recover.go replaces a task whose
// runner never took its job, re-runs a job a Spot reclaim killed, and logs
// the events the stack's alarms count.
package runners

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/t-d-d/gh-paddock/internal/repos"
)

// ConfigPath is where runner configs live: one parameter per runner, under
// its scale set's id.
const ConfigPath = "/paddock/runners"

// TaskFamily is the family of the one-off runner task definitions.
const TaskFamily = "paddock-runner"

// configGrace is how long a config is kept with no pending task to show
// for it, since ListTasks can lag a RunTask.
const configGrace = 5 * time.Minute

// WarmLimit is how long after its start a task may still be handed a
// config. A job running at the limit finishes, then the task exits.
const WarmLimit = 30 * time.Minute

// urlLife is how long a task's URL is signed for: a little over the warm
// limit, so a config handed at the limit can still be read.
const urlLife = WarmLimit + 5*time.Minute

// handWithin is how long after its runner's job finishes a task may be
// handed a config. The task waits 30 seconds from when its runner leaves,
// which follows the finish; the margin leaves it time to read the config.
const handWithin = 25 * time.Second

// logTaskTimes is the line that gives how long Fargate took to start a
// task: to place it, to pull its image, and to start its container.
const logTaskTimes = "runner task start times"

// logStartRefused is the line for a runner task ECS would not start, such
// as one past the account's Fargate quota. The scale stops there, the
// session goes on, and a later scale tries again.
const logStartRefused = "runner task refused"

// heardFor is how long the scaler keeps what a message said of a runner, for
// a task that ListTasks has yet to show.
const heardFor = 2 * time.Minute

// ECS is the part of the ECS client the scaler calls.
type ECS interface {
	ListTasks(context.Context, *ecs.ListTasksInput, ...func(*ecs.Options)) (*ecs.ListTasksOutput, error)
	DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	RegisterTaskDefinition(context.Context, *ecs.RegisterTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error)
	DeregisterTaskDefinition(context.Context, *ecs.DeregisterTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.DeregisterTaskDefinitionOutput, error)
	DeleteTaskDefinitions(context.Context, *ecs.DeleteTaskDefinitionsInput, ...func(*ecs.Options)) (*ecs.DeleteTaskDefinitionsOutput, error)
	RunTask(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
	StopTask(context.Context, *ecs.StopTaskInput, ...func(*ecs.Options)) (*ecs.StopTaskOutput, error)
	TagResource(context.Context, *ecs.TagResourceInput, ...func(*ecs.Options)) (*ecs.TagResourceOutput, error)
}

// SSM is the part of the Parameter Store client the scaler calls.
type SSM interface {
	PutParameter(context.Context, *ssm.PutParameterInput, ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	DeleteParameter(context.Context, *ssm.DeleteParameterInput, ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
	GetParametersByPath(context.Context, *ssm.GetParametersByPathInput, ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
}

// JIT makes a runner's just-in-time config, and removes the record a config
// made of a runner that never took its job.
type JIT interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error)
	RemoveRunner(context.Context, int64) error
}

// Objects is the part of the S3 client the scaler calls: it writes a task's
// later config to the task's object, and deletes it.
type Objects interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// Presigner signs the URL that lets a task read its own object.
type Presigner interface {
	PresignGetObject(context.Context, *s3.GetObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// Launch is where and how every runner task runs, from the stack.
type Launch struct {
	Cluster          string
	Image            string
	ExecutionRoleARN string
	LogGroup         string
	Region           string
	Subnets          []string
	SecurityGroup    string
	// Bucket is the runner-config bucket, which holds each task's later
	// configs.
	Bucket string
}

// Scaler keeps one pool's runner tasks matched to its assigned jobs.
type Scaler struct {
	ECS       ECS
	SSM       SSM
	JIT       JIT
	Objects   Objects
	Presigner Presigner
	Launch    Launch
	Repo      repos.Repo
	Pool      repos.Pool
	// Settings, if set, is the pool's settings as the list last gave them.
	// Each Scale takes them into Pool first, so a change applies to the
	// next task started.
	Settings   *repos.Current
	ScaleSetID int
	Log        *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Actions re-runs a job a Spot reclaim killed; nil re-runs none.
	Actions Actions
	// Metrics reads the pool's 30-day counts; nil reads none.
	Metrics Metrics

	last *scaleset.RunnerScaleSetStatistic
	// tasks is what the scaler knows of each live task, by its ARN. A
	// restart loses it: the scaler then knows a task only by its first
	// runner, so a task on a later runner counts as busy until it exits, as
	// every task did before warm reuse.
	tasks map[string]*task
	// finished and startedJobs are the runners messages said finished or
	// took a job, and when.
	finished    map[string]time.Time
	startedJobs map[string]jobNote

	recovery
}

// task is what the scaler knows of one of its pool's tasks.
type task struct {
	arn     string
	key     string    // its object in the bucket, which its URL reaches
	runner  string    // the runner it was last handed
	created time.Time // when it started, for the warm limit
	// pending is true while its object holds a config whose runner has not
	// been seen to register.
	pending bool
	// done is when its runner's job finished, zero while the runner has a
	// job or is starting.
	done time.Time
	// handed is when its runner's config was handed over, at its start or
	// through its URL, and started is whether that runner took its job.
	handed  time.Time
	started bool
	// runUnknown is true while its runner is known to run a job whose run
	// no message named, and none has been found.
	runUnknown bool
	// warned is when it was last logged as past its pool's job time.
	warned time.Time
	// maxJob is its pool's maximum job time when it started; zero for a
	// task started before the tag, which takes the pool's.
	maxJob time.Duration
	// timed is whether its start times have been logged. A restart loses
	// it, so a live task may be logged again.
	timed bool
}

func (s *Scaler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scaler) startedBy() string { return startedBy(s.ScaleSetID) }

// startedBy marks the tasks started for a scale set, so each pool counts
// its own.
func startedBy(scaleSetID int) string { return "paddock-" + strconv.Itoa(scaleSetID) }

// LiveTasks counts the tasks started for a scale set that have not been
// asked to stop: a busy task, a waiting one, or one still starting.
func LiveTasks(ctx context.Context, client ECS, cluster string, scaleSetID int) (int, error) {
	arns, err := listTasks(ctx, client, cluster, startedBy(scaleSetID), ecstypes.DesiredStatusRunning)
	return len(arns), err
}

func (s *Scaler) configDir() string { return ConfigPath + "/" + strconv.Itoa(s.ScaleSetID) }

// objectKey is the object that holds a task's later configs, named for the
// task's first runner.
func (s *Scaler) objectKey(firstRunner string) string {
	return strconv.Itoa(s.ScaleSetID) + "/" + firstRunner
}

// Scale finds a runner for each assigned job that has none, up to the
// pool's maximum: first by handing a config to a task waiting after its
// last job, then by starting a new task. GitHub's statistics count every
// job assigned to the set, running or waiting, and ECS counts every task
// that has not stopped, so a listener that restarts starts only what is
// missing. A poll that carried no message means GitHub's counts have not
// changed, so it scales on the last counts: that replaces a task that
// stopped before it took its job, or exited before it read its config.
// When ECS refuses a start, Scale logs it and returns, so the session goes
// on reading messages and a later scale tries again.
func (s *Scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if s.Settings != nil {
		s.Pool = s.Settings.Get()
	}
	s.hear(msg)
	if msg != nil && msg.Statistics != nil {
		s.last = msg.Statistics
	}
	// Before any statistics, nothing is scaled, but failed starts and
	// reclaims are still read back from ECS.
	stats := s.last
	if stats == nil {
		stats = &scaleset.RunnerScaleSetStatistic{}
	}
	live, err := s.tasksOf(ctx, ecstypes.DesiredStatusRunning)
	if err != nil {
		return err
	}
	if err := s.sweepConfigs(ctx, live); err != nil {
		s.Log.Warn("could not clear runner configs", "error", err.Error())
	}
	s.track(ctx, live)
	live = s.replaceFailedStarts(ctx, live)
	s.watch(ctx)

	now := s.now()
	busy := 0
	var waiting []*task
	for _, t := range s.tasks {
		switch {
		case t.done.IsZero():
			busy++
		case now.Sub(t.done) < handWithin && now.Sub(t.created) < WarmLimit:
			waiting = append(waiting, t)
		}
	}
	need := min(stats.TotalAssignedJobs, s.Pool.MaxRunners) - busy
	for _, t := range waiting {
		if need <= 0 {
			break
		}
		if err := s.hand(ctx, t); err != nil {
			return err
		}
		need--
	}
	for have := len(live); need > 0 && have < s.Pool.MaxRunners; have++ {
		err := s.start(ctx)
		var r refused
		if errors.As(err, &r) {
			// The next start would likely be refused too. The jobs still
			// count as assigned, so a later scale starts their tasks.
			s.Log.Warn(logStartRefused, "reason", r.reason, "missing", need)
			return nil
		}
		if err != nil {
			return err
		}
		need--
	}
	return nil
}

// refused is ECS declining to start a runner task, with a failure in its
// answer, such as at the account's Fargate quota or with no Spot capacity.
// It ends a scale, not the session.
type refused struct{ reason string }

func (r refused) Error() string { return "ECS started no runner task: " + r.reason }

// jobNote is what a message said of a job a runner took: when it was heard,
// and the job's workflow run.
type jobNote struct {
	at    time.Time
	runID int64
}

// hear notes which runners a message says took a job or finished one, and
// which jobs wait for a runner. Each finished job is logged, for its pool's
// count.
func (s *Scaler) hear(msg *scaleset.RunnerScaleSetMessage) {
	if s.finished == nil {
		s.finished, s.startedJobs = map[string]time.Time{}, map[string]jobNote{}
	}
	now := s.now()
	for runner, at := range s.finished {
		if now.Sub(at) > heardFor {
			delete(s.finished, runner)
		}
	}
	for runner, note := range s.startedJobs {
		if now.Sub(note.at) > heardFor {
			delete(s.startedJobs, runner)
		}
	}
	if msg == nil {
		return
	}
	s.hearWaits(msg)
	for _, job := range msg.JobStartedMessages {
		s.startedJobs[job.RunnerName] = jobNote{at: now, runID: job.WorkflowRunID}
		s.failedInARow = 0
	}
	for _, job := range msg.JobCompletedMessages {
		s.finished[job.RunnerName] = now
		s.Log.Info(logJobFinished, "runner", job.RunnerName, "result", job.Result)
	}
}

// track brings what the scaler knows of its tasks up to date with the live
// ones. A gone task's object is deleted; so is one whose runner has
// registered and taken its job. A task whose runner was heard to finish is
// waiting from then.
func (s *Scaler) track(ctx context.Context, live []ecstypes.Task) {
	if s.tasks == nil {
		s.tasks = map[string]*task{}
	}
	seen := map[string]bool{}
	for _, lt := range live {
		arn := aws.ToString(lt.TaskArn)
		seen[arn] = true
		t, ok := s.tasks[arn]
		if !ok {
			t = s.readTask(lt)
			s.tasks[arn] = t
		}
		s.logTimes(t, lt)
	}
	for arn, t := range s.tasks {
		if !seen[arn] {
			if t.pending {
				s.deleteObject(ctx, t)
			}
			delete(s.tasks, arn)
			continue
		}
		// GitHub keeps a record of a runner from the moment its config is
		// made, so the record cannot show the runner registered. A
		// just-in-time runner takes its job as soon as it registers, so
		// its job's start is the sign; its finish, or its task gone, is
		// the backstop.
		note, took := s.startedJobs[t.runner]
		_, ended := s.finished[t.runner]
		if t.pending && (took || ended) {
			s.deleteObject(ctx, t)
		}
		if took && (!t.started || t.runUnknown) {
			s.tag(ctx, t, tagJob, t.runner+"@"+strconv.FormatInt(note.runID, 10))
			t.runUnknown = false
		}
		if took || ended {
			t.started = true
		}
		if at, ok := s.finished[t.runner]; ok && t.done.IsZero() {
			t.done = at
		}
	}
}

// logTimes logs, once its container has started, how long Fargate took to
// place a task, pull its image and start its container. ECS keeps these
// times in its task description alone, which only the owner can read. The
// line names the task by its id and its first runner, never by an ARN or
// the image (docs/constraints/public-repo.md).
func (s *Scaler) logTimes(t *task, lt ecstypes.Task) {
	if t.timed || lt.CreatedAt == nil || lt.PullStartedAt == nil || lt.PullStoppedAt == nil || lt.StartedAt == nil {
		return
	}
	t.timed = true
	seconds := func(from, to *time.Time) float64 {
		return to.Sub(*from).Round(100 * time.Millisecond).Seconds()
	}
	s.Log.Info(logTaskTimes,
		"task", shortARN(t.arn),
		"runner", tagOf(lt, "paddock:runner"),
		"placementSeconds", seconds(lt.CreatedAt, lt.PullStartedAt),
		"pullSeconds", seconds(lt.PullStartedAt, lt.PullStoppedAt),
		"startSeconds", seconds(lt.PullStoppedAt, lt.StartedAt),
	)
}

func (s *Scaler) deleteObject(ctx context.Context, t *task) {
	if _, err := s.Objects.DeleteObject(context.WithoutCancel(ctx), &s3.DeleteObjectInput{
		Bucket: aws.String(s.Launch.Bucket),
		Key:    aws.String(t.key),
	}); err != nil {
		// The bucket expires it in a day.
		s.Log.Warn("could not delete a later runner config", "runner", t.runner, "error", err.Error())
		return
	}
	t.pending = false
}

// laterConfig is what a task's object holds: the runner's name, which the
// task checks so it never runs a config twice, and its config.
type laterConfig struct {
	Runner    string `json:"runner"`
	JITConfig string `json:"jitconfig"`
}

// firstConfig is what a task's parameter holds: its first config, and the
// URL to its object.
type firstConfig struct {
	laterConfig
	URL string `json:"url"`
}

// hand writes a new runner's config to a waiting task's object, which the
// task polls through its URL.
func (s *Scaler) hand(ctx context.Context, t *task) error {
	runner, jit, err := s.newRunner(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(laterConfig{Runner: runner, JITConfig: jit})
	if err != nil {
		return err
	}
	if _, err := s.Objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.Launch.Bucket),
		Key:         aws.String(t.key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	}); err != nil {
		return fmt.Errorf("hand a waiting task its config: %w", err)
	}
	t.runner, t.pending, t.done = runner, true, time.Time{}
	t.handed, t.started = s.now(), false
	s.tag(ctx, t, tagHanded, runner+"@"+strconv.FormatInt(t.handed.Unix(), 10))
	s.Log.Info("config handed to a waiting task", "runner", runner, "task", t.key[strings.LastIndex(t.key, "/")+1:])
	return nil
}

// newRunner names a runner and makes its config, for this pool's scale set
// alone.
func (s *Scaler) newRunner(ctx context.Context) (name, jit string, err error) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", "", err
	}
	name = "paddock-" + hex.EncodeToString(suffix)
	cfg, err := s.JIT.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: "_work"}, s.ScaleSetID)
	if err != nil {
		return "", "", fmt.Errorf("make a runner's config: %w", err)
	}
	return name, cfg.EncodedJITConfig, nil
}

// tasksOf lists the pool's tasks of one desired status, with their tags.
// ECS shows a stopped task for about an hour.
func (s *Scaler) tasksOf(ctx context.Context, status ecstypes.DesiredStatus) ([]ecstypes.Task, error) {
	arns, err := s.taskARNs(ctx, status)
	if err != nil {
		return nil, err
	}
	return s.describeTasks(ctx, arns)
}

// taskARNs lists the ARNs of the pool's tasks of one desired status.
func (s *Scaler) taskARNs(ctx context.Context, status ecstypes.DesiredStatus) ([]string, error) {
	return listTasks(ctx, s.ECS, s.Launch.Cluster, s.startedBy(), status)
}

func listTasks(ctx context.Context, client ECS, cluster, by string, status ecstypes.DesiredStatus) ([]string, error) {
	var arns []string
	var next *string
	for {
		out, err := client.ListTasks(ctx, &ecs.ListTasksInput{
			Cluster:       aws.String(cluster),
			StartedBy:     aws.String(by),
			DesiredStatus: status,
			NextToken:     next,
		})
		if err != nil {
			return nil, fmt.Errorf("list the pool's tasks: %w", err)
		}
		arns = append(arns, out.TaskArns...)
		if next = out.NextToken; next == nil {
			return arns, nil
		}
	}
}

// describeTasks describes tasks by their ARNs, with their tags.
func (s *Scaler) describeTasks(ctx context.Context, arns []string) ([]ecstypes.Task, error) {
	var tasks []ecstypes.Task
	for len(arns) > 0 {
		batch := arns[:min(len(arns), 100)]
		arns = arns[len(batch):]
		out, err := s.ECS.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: aws.String(s.Launch.Cluster),
			Tasks:   batch,
			Include: []ecstypes.TaskField{ecstypes.TaskFieldTags},
		})
		if err != nil {
			return nil, fmt.Errorf("describe the pool's tasks: %w", err)
		}
		tasks = append(tasks, out.Tasks...)
	}
	return tasks, nil
}

// sweepConfigs deletes each config whose task no longer waits for it: one
// that runs has had its config injected, and one that has gone never will.
func (s *Scaler) sweepConfigs(ctx context.Context, tasks []ecstypes.Task) error {
	waiting := map[string]bool{}
	for _, t := range tasks {
		switch aws.ToString(t.LastStatus) {
		case "PROVISIONING", "PENDING", "ACTIVATING":
			waiting[tagOf(t, "paddock:runner")] = true
		}
	}
	var next *string
	for {
		out, err := s.SSM.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
			Path:           aws.String(s.configDir()),
			WithDecryption: aws.Bool(false),
			NextToken:      next,
		})
		if err != nil {
			return err
		}
		for _, p := range out.Parameters {
			name := aws.ToString(p.Name)
			runner := name[strings.LastIndex(name, "/")+1:]
			if waiting[runner] || s.now().Sub(aws.ToTime(p.LastModifiedDate)) < configGrace {
				continue
			}
			if err := s.deleteConfig(ctx, name); err != nil {
				return err
			}
		}
		if next = out.NextToken; next == nil {
			return nil
		}
	}
}

func (s *Scaler) deleteConfig(ctx context.Context, name string) error {
	_, err := s.SSM.DeleteParameter(context.WithoutCancel(ctx), &ssm.DeleteParameterInput{Name: aws.String(name)})
	var missing *ssmtypes.ParameterNotFound
	if errors.As(err, &missing) {
		return nil
	}
	return err
}

func tagOf(t ecstypes.Task, key string) string {
	for _, tag := range t.Tags {
		if aws.ToString(tag.Key) == key {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}

func (s *Scaler) start(ctx context.Context) (err error) {
	runner, jit, err := s.newRunner(ctx)
	if err != nil {
		return err
	}
	// Signed once, now, and never again: the task reads its later configs
	// with it until the warm limit.
	signed, err := s.Presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Launch.Bucket),
		Key:    aws.String(s.objectKey(runner)),
	}, s3.WithPresignExpires(urlLife))
	if err != nil {
		return fmt.Errorf("sign a task's config URL: %w", err)
	}
	value, err := json.Marshal(firstConfig{laterConfig{Runner: runner, JITConfig: jit}, signed.URL})
	if err != nil {
		return err
	}
	param := s.configDir() + "/" + runner
	if _, err := s.SSM.PutParameter(ctx, &ssm.PutParameterInput{
		Name:  aws.String(param),
		Value: aws.String(string(value)),
		Type:  ssmtypes.ParameterTypeSecureString,
		// A config can pass the standard tier's 4 KB; it lives minutes.
		Tier: ssmtypes.ParameterTierIntelligentTiering,
	}); err != nil {
		return fmt.Errorf("write a runner's config: %w", err)
	}
	started := false
	defer func() {
		if !started {
			if derr := s.deleteConfig(ctx, param); derr != nil {
				s.Log.Warn("could not delete an unused runner config", "runner", runner, "error", derr.Error())
			}
		}
	}()

	tags := []ecstypes.Tag{
		{Key: aws.String("paddock:repo"), Value: aws.String(s.Repo.FullName())},
		{Key: aws.String("paddock:pool"), Value: aws.String(s.Pool.Name)},
		{Key: aws.String("paddock:runner"), Value: aws.String(runner)},
	}
	if s.Pool.MaxJob > 0 {
		tags = append(tags, ecstypes.Tag{Key: aws.String(tagMaxJob), Value: aws.String(strconv.Itoa(int(s.Pool.MaxJob / time.Minute)))})
	}
	def, err := s.ECS.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family:                  aws.String(TaskFamily),
		RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
		NetworkMode:             ecstypes.NetworkModeAwsvpc,
		Cpu:                     aws.String(strconv.Itoa(s.Pool.VCPU * 1024)),
		Memory:                  aws.String(strconv.Itoa(s.Pool.MemoryGB * 1024)),
		RuntimePlatform: &ecstypes.RuntimePlatform{
			CpuArchitecture:       cpuArchitecture(s.Pool.Arch),
			OperatingSystemFamily: ecstypes.OSFamilyLinux,
		},
		ExecutionRoleArn: aws.String(s.Launch.ExecutionRoleARN),
		ContainerDefinitions: []ecstypes.ContainerDefinition{{
			Name:      aws.String("runner"),
			Image:     aws.String(s.Launch.Image),
			Essential: aws.Bool(true),
			// Fargate Spot's two minutes' notice, the most Fargate allows:
			// the entrypoint holds the stop back from a running job.
			StopTimeout: aws.Int32(120),
			Secrets:     []ecstypes.Secret{{Name: aws.String("PADDOCK_CONFIG"), ValueFrom: aws.String(param)}},
			LogConfiguration: &ecstypes.LogConfiguration{
				LogDriver: ecstypes.LogDriverAwslogs,
				Options: map[string]string{
					"awslogs-group":         s.Launch.LogGroup,
					"awslogs-region":        s.Launch.Region,
					"awslogs-stream-prefix": "runner",
				},
			},
		}},
		Tags: tags,
	})
	if err != nil {
		return fmt.Errorf("register a runner's task definition: %w", err)
	}
	// A task keeps running on a definition deleted under it, so the one-off
	// definition goes as soon as the task is asked for.
	defArn := def.TaskDefinition.TaskDefinitionArn
	defer func() {
		bg := context.WithoutCancel(ctx)
		if _, derr := s.ECS.DeregisterTaskDefinition(bg, &ecs.DeregisterTaskDefinitionInput{TaskDefinition: defArn}); derr != nil {
			s.Log.Warn("could not deregister a runner's task definition", "runner", runner, "error", derr.Error())
			return
		}
		if _, derr := s.ECS.DeleteTaskDefinitions(bg, &ecs.DeleteTaskDefinitionsInput{TaskDefinitions: []string{aws.ToString(defArn)}}); derr != nil {
			s.Log.Warn("could not delete a runner's task definition", "runner", runner, "error", derr.Error())
		}
	}()

	provider := "FARGATE_SPOT"
	if s.Pool.Capacity == repos.OnDemand {
		provider = "FARGATE"
	}
	out, err := s.ECS.RunTask(ctx, &ecs.RunTaskInput{
		Cluster:                  aws.String(s.Launch.Cluster),
		TaskDefinition:           defArn,
		Count:                    aws.Int32(1),
		StartedBy:                aws.String(s.startedBy()),
		CapacityProviderStrategy: []ecstypes.CapacityProviderStrategyItem{{CapacityProvider: aws.String(provider), Weight: 1}},
		NetworkConfiguration: &ecstypes.NetworkConfiguration{AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
			Subnets:        s.Launch.Subnets,
			SecurityGroups: []string{s.Launch.SecurityGroup},
			AssignPublicIp: ecstypes.AssignPublicIpEnabled,
		}},
		Tags: tags,
	})
	if err != nil {
		// An error from the call itself, such as a denied role or a bad
		// subnet, is not one a later scale gets past, so it ends the
		// session. ECS reports a quota or capacity refusal as a failure.
		return fmt.Errorf("start a runner task: %w", err)
	}
	if len(out.Tasks) == 0 {
		reason := "no reason given"
		if len(out.Failures) > 0 {
			reason = aws.ToString(out.Failures[0].Reason)
		}
		return refused{reason}
	}
	started = true
	arn := aws.ToString(out.Tasks[0].TaskArn)
	// The caller's logger carries the repo and the pool.
	s.Log.Info("runner task started", "runner", runner, "task", arn[strings.LastIndex(arn, "/")+1:], "capacity", provider, "arch", string(cpuArchitecture(s.Pool.Arch)))
	return nil
}

// cpuArchitecture is the Fargate architecture of a pool's runner tasks. The
// list gives every pool an arch, x64 unless it sets arm64.
func cpuArchitecture(a repos.Arch) ecstypes.CPUArchitecture {
	if a == repos.ARM64 {
		return ecstypes.CPUArchitectureArm64
	}
	return ecstypes.CPUArchitectureX8664
}
