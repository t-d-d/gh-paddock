package runners

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
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

// fakeAWS is ECS, Parameter Store and S3, in memory.
type fakeAWS struct {
	mu        sync.Mutex
	tasks     []ecstypes.Task
	objects   map[string]string
	objPuts   []string
	presigned []presign
	defs      map[string]*ecs.RegisterTaskDefinitionInput
	nextDef   int
	deleted   []string
	runs      []*ecs.RunTaskInput
	params    map[string]ssmtypes.Parameter
	paramPuts []*ssm.PutParameterInput
	failRun   bool
	// runErr, if set, is the error RunTask returns.
	runErr error
	// events is what was done to runners and tasks, in order: "remove
	// <runner>" and "stop <first runner>".
	events  []string
	stopped []string
	// described is every task ARN asked of DescribeTasks, in order.
	described []string
}

func newFakeAWS() *fakeAWS {
	return &fakeAWS{defs: map[string]*ecs.RegisterTaskDefinitionInput{}, params: map[string]ssmtypes.Parameter{}, objects: map[string]string{}}
}

type presign struct {
	bucket, key string
	expires     time.Duration
}

func (f *fakeAWS) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body bytes.Buffer
	if _, err := io.Copy(&body, in.Body); err != nil {
		return nil, err
	}
	key := aws.ToString(in.Bucket) + "/" + aws.ToString(in.Key)
	f.objects[key] = body.String()
	f.objPuts = append(f.objPuts, key)
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeAWS) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeAWS) PresignGetObject(_ context.Context, in *s3.GetObjectInput, opts ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var o s3.PresignOptions
	for _, opt := range opts {
		opt(&o)
	}
	f.presigned = append(f.presigned, presign{aws.ToString(in.Bucket), aws.ToString(in.Key), o.Expires})
	return &v4.PresignedHTTPRequest{URL: "https://" + aws.ToString(in.Bucket) + ".s3.example/" + aws.ToString(in.Key) + "?X-Amz-Signature=sig", Method: "GET"}, nil
}

// object reads a task's later config, as its URL would.
func (f *fakeAWS) object(t *testing.T, key string) (later struct{ Runner, JITConfig string }, ok bool) {
	t.Helper()
	f.mu.Lock()
	raw, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		return later, false
	}
	var v struct {
		Runner    string `json:"runner"`
		JITConfig string `json:"jitconfig"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("object %s: %v", key, err)
	}
	return struct{ Runner, JITConfig string }{v.Runner, v.JITConfig}, true
}

func (f *fakeAWS) ListTasks(_ context.Context, in *ecs.ListTasksInput, _ ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.DesiredStatus != ecstypes.DesiredStatusRunning && in.DesiredStatus != ecstypes.DesiredStatusStopped {
		return nil, fmt.Errorf("listed tasks of desired status %q", in.DesiredStatus)
	}
	var arns []string
	for _, t := range f.tasks {
		if aws.ToString(t.StartedBy) == aws.ToString(in.StartedBy) && aws.ToString(t.DesiredStatus) == string(in.DesiredStatus) {
			arns = append(arns, aws.ToString(t.TaskArn))
		}
	}
	return &ecs.ListTasksOutput{TaskArns: arns}, nil
}

func (f *fakeAWS) DescribeTasks(_ context.Context, in *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ecstypes.Task
	f.described = append(f.described, in.Tasks...)
	for _, arn := range in.Tasks {
		for _, t := range f.tasks {
			if aws.ToString(t.TaskArn) == arn {
				out = append(out, t)
			}
		}
	}
	return &ecs.DescribeTasksOutput{Tasks: out}, nil
}

func (f *fakeAWS) RegisterTaskDefinition(_ context.Context, in *ecs.RegisterTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextDef++
	arn := fmt.Sprintf("arn:aws:ecs:us-east-1:123456789012:task-definition/%s:%d", aws.ToString(in.Family), f.nextDef)
	f.defs[arn] = in
	return &ecs.RegisterTaskDefinitionOutput{TaskDefinition: &ecstypes.TaskDefinition{TaskDefinitionArn: aws.String(arn)}}, nil
}

func (f *fakeAWS) DeregisterTaskDefinition(_ context.Context, in *ecs.DeregisterTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DeregisterTaskDefinitionOutput, error) {
	return &ecs.DeregisterTaskDefinitionOutput{}, nil
}

func (f *fakeAWS) DeleteTaskDefinitions(_ context.Context, in *ecs.DeleteTaskDefinitionsInput, _ ...func(*ecs.Options)) (*ecs.DeleteTaskDefinitionsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, in.TaskDefinitions...)
	return &ecs.DeleteTaskDefinitionsOutput{}, nil
}

func (f *fakeAWS) RunTask(_ context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, in)
	if f.runErr != nil {
		return nil, f.runErr
	}
	if f.failRun {
		return &ecs.RunTaskOutput{Failures: []ecstypes.Failure{{Reason: aws.String("RESOURCE:FARGATE")}}}, nil
	}
	task := ecstypes.Task{
		TaskArn:       aws.String(fmt.Sprintf("arn:aws:ecs:us-east-1:123456789012:task/paddock/%032d", len(f.tasks)+1)),
		StartedBy:     in.StartedBy,
		DesiredStatus: aws.String("RUNNING"),
		LastStatus:    aws.String("PROVISIONING"),
		CreatedAt:     aws.Time(time.Now()),
		Tags:          in.Tags,
	}
	f.tasks = append(f.tasks, task)
	return &ecs.RunTaskOutput{Tasks: []ecstypes.Task{task}}, nil
}

func (f *fakeAWS) StopTask(_ context.Context, in *ecs.StopTaskInput, _ ...func(*ecs.Options)) (*ecs.StopTaskOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if aws.ToString(in.Cluster) != "paddock" {
		return nil, fmt.Errorf("stopped a task in cluster %q", aws.ToString(in.Cluster))
	}
	for i, t := range f.tasks {
		if aws.ToString(t.TaskArn) == aws.ToString(in.Task) {
			f.tasks[i].DesiredStatus = aws.String("STOPPED")
			f.tasks[i].StopCode = ecstypes.TaskStopCodeUserInitiated
			f.stopped = append(f.stopped, aws.ToString(in.Task))
			f.events = append(f.events, "stop "+tagOf(t, "paddock:runner"))
			return &ecs.StopTaskOutput{}, nil
		}
	}
	return nil, errors.New("no such task")
}

func (f *fakeAWS) TagResource(_ context.Context, in *ecs.TagResourceInput, _ ...func(*ecs.Options)) (*ecs.TagResourceOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, t := range f.tasks {
		if aws.ToString(t.TaskArn) != aws.ToString(in.ResourceArn) {
			continue
		}
		for _, tag := range in.Tags {
			kept := t.Tags[:0:0]
			for _, old := range f.tasks[i].Tags {
				if aws.ToString(old.Key) != aws.ToString(tag.Key) {
					kept = append(kept, old)
				}
			}
			f.tasks[i].Tags = append(kept, tag)
		}
		return &ecs.TagResourceOutput{}, nil
	}
	return nil, errors.New("no such task")
}

func (f *fakeAWS) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paramPuts = append(f.paramPuts, in)
	f.params[aws.ToString(in.Name)] = ssmtypes.Parameter{Name: in.Name, LastModifiedDate: aws.Time(time.Now())}
	return &ssm.PutParameterOutput{}, nil
}

func (f *fakeAWS) DeleteParameter(_ context.Context, in *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.params[aws.ToString(in.Name)]; !ok {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	delete(f.params, aws.ToString(in.Name))
	return &ssm.DeleteParameterOutput{}, nil
}

func (f *fakeAWS) GetParametersByPath(_ context.Context, in *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if aws.ToBool(in.WithDecryption) {
		return nil, errors.New("the sweep decrypted runner configs it only deletes")
	}
	var out []ssmtypes.Parameter
	for name, p := range f.params {
		if strings.HasPrefix(name, aws.ToString(in.Path)+"/") {
			out = append(out, p)
		}
	}
	return &ssm.GetParametersByPathOutput{Parameters: out}, nil
}

// stop ends every task: its job is done and its runner exited.
func (f *fakeAWS) stopAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.tasks {
		f.tasks[i].DesiredStatus = aws.String("STOPPED")
		f.tasks[i].LastStatus = aws.String("STOPPED")
	}
}

func (f *fakeAWS) setLastStatus(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.tasks {
		f.tasks[i].LastStatus = aws.String(status)
	}
}

type fakeJIT struct {
	calls        []string
	removed      []string
	refuseRemove bool
	// jobRunning refuses each removal as GitHub does a busy runner's.
	jobRunning  bool
	removeTries int
	aws         *fakeAWS // where removals are logged beside the stops
}

func (j *fakeJIT) GetRunnerByName(_ context.Context, name string) (*scaleset.RunnerReference, error) {
	for i, c := range j.calls {
		if c == name {
			return &scaleset.RunnerReference{ID: i + 1, Name: name, RunnerScaleSetID: 5}, nil
		}
	}
	return nil, nil
}

func (j *fakeJIT) RemoveRunner(_ context.Context, id int64) error {
	j.removeTries++
	if j.jobRunning {
		return fmt.Errorf("remove runner: %w", scaleset.JobStillRunningError)
	}
	if j.refuseRemove {
		return errors.New("the runner is running a job")
	}
	name := j.calls[id-1]
	j.removed = append(j.removed, name)
	if j.aws != nil {
		j.aws.mu.Lock()
		j.aws.events = append(j.aws.events, "remove "+name)
		j.aws.mu.Unlock()
	}
	return nil
}

func (j *fakeJIT) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, id int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	j.calls = append(j.calls, s.Name)
	return &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "jit-for-" + s.Name}, nil
}

func newScaler(f *fakeAWS, pool repos.Pool) (*Scaler, *fakeJIT) {
	jit := &fakeJIT{}
	return &Scaler{
		ECS: f, SSM: f, JIT: jit, Objects: f, Presigner: f,
		Launch: Launch{
			Cluster: "paddock", Image: "registry.example/runner@sha256:abc", ExecutionRoleARN: "arn:aws:iam::123456789012:role/runner-exec",
			LogGroup: "/paddock/runners", Region: "us-east-1", Subnets: []string{"subnet-1", "subnet-2"}, SecurityGroup: "sg-1",
			Bucket: "configs",
		},
		Repo:       repos.Repo{Owner: "acme", Name: "widgets"},
		Pool:       pool,
		ScaleSetID: 5,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, jit
}

func assigned(n int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: n}}
}

var spotTest = repos.Pool{Name: "test", Capacity: repos.Spot, VCPU: 2, MemoryGB: 8, MaxRunners: 10}

func TestAnAssignedJobStartsARunnerTaskOfThePoolsCapacityAndSize(t *testing.T) {
	t.Run("fitness: a pool with no arch runs its runner tasks on x64, counted to its own pool (runner-image.md, pool-per-repo.md)", func(t *testing.T) {
		f := newFakeAWS()
		s, jit := newScaler(f, repos.Pool{Name: "dev", Capacity: repos.OnDemand, VCPU: 4, MemoryGB: 16, MaxRunners: 10})
		if err := s.Scale(context.Background(), assigned(1)); err != nil {
			t.Fatal(err)
		}
		if len(f.runs) != 1 || len(jit.calls) != 1 {
			t.Fatalf("runs %d, jit configs %d", len(f.runs), len(jit.calls))
		}
		run := f.runs[0]
		if got := run.CapacityProviderStrategy; len(got) != 1 || aws.ToString(got[0].CapacityProvider) != "FARGATE" {
			t.Fatalf("capacity %+v", got)
		}
		def := f.defs[aws.ToString(run.TaskDefinition)]
		if def == nil {
			t.Fatal("the task ran a definition that was not registered")
		}
		if aws.ToString(def.Cpu) != "4096" || aws.ToString(def.Memory) != "16384" {
			t.Fatalf("size %s/%s", aws.ToString(def.Cpu), aws.ToString(def.Memory))
		}
		if def.RuntimePlatform.CpuArchitecture != ecstypes.CPUArchitectureX8664 {
			t.Fatalf("architecture %q", def.RuntimePlatform.CpuArchitecture)
		}
		if aws.ToString(run.StartedBy) != "paddock-5" || aws.ToString(run.Cluster) != "paddock" {
			t.Fatalf("startedBy %q in %q", aws.ToString(run.StartedBy), aws.ToString(run.Cluster))
		}
		vpc := run.NetworkConfiguration.AwsvpcConfiguration
		if vpc.AssignPublicIp != ecstypes.AssignPublicIpEnabled || len(vpc.Subnets) != 2 || vpc.SecurityGroups[0] != "sg-1" {
			t.Fatalf("network %+v", vpc)
		}
		tags := tagMap(run.Tags)
		if tags["paddock:repo"] != "acme/widgets" || tags["paddock:pool"] != "dev" || tags["paddock:runner"] != jit.calls[0] {
			t.Fatalf("tags %v", tags)
		}
	})
}

func TestAnArm64PoolsRunnerTasksRunOnArm64(t *testing.T) {
	f := newFakeAWS()
	s, _ := newScaler(f, repos.Pool{Name: "arm", Capacity: repos.Spot, Arch: repos.ARM64, VCPU: 2, MemoryGB: 8, MaxRunners: 10})
	if err := s.Scale(context.Background(), assigned(1)); err != nil {
		t.Fatal(err)
	}
	def := f.defs[aws.ToString(f.runs[0].TaskDefinition)]
	if def.RuntimePlatform.CpuArchitecture != ecstypes.CPUArchitectureArm64 {
		t.Fatalf("architecture %q", def.RuntimePlatform.CpuArchitecture)
	}
}

func TestSpotPoolsRunOnFargateSpot(t *testing.T) {
	f := newFakeAWS()
	s, _ := newScaler(f, spotTest)
	if err := s.Scale(context.Background(), assigned(1)); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(f.runs[0].CapacityProviderStrategy[0].CapacityProvider); got != "FARGATE_SPOT" {
		t.Fatalf("capacity %s", got)
	}
}

// tagMap reads tags as a map of key to value.
func tagMap(tags []ecstypes.Tag) map[string]string {
	m := map[string]string{}
	for _, tag := range tags {
		m[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return m
}

// Cost Explorer reads a repo's and a pool's runner cost from these two
// tags, once the operator turns them on as cost allocation tags. A task
// started without them shows as cost of no repo.
func TestEveryRunnerTaskIsTaggedWithItsRepoAndPool(t *testing.T) {
	pools := []repos.Pool{spotTest, {Name: "dev", Capacity: repos.OnDemand, VCPU: 4, MemoryGB: 16, MaxRunners: 10}}
	for _, pool := range pools {
		t.Run("fitness: every runner task of a "+pool.Name+" pool is tagged with its repo and pool (pool-per-repo.md)", func(t *testing.T) {
			f := newFakeAWS()
			s, _ := newScaler(f, pool)
			s.Repo = repos.Repo{Owner: "acme", Name: "gadgets"}
			ctx := context.Background()
			if err := s.Scale(ctx, assigned(3)); err != nil {
				t.Fatal(err)
			}
			// The tasks finish, and the next jobs get new ones.
			f.stopAll()
			if err := s.Scale(ctx, assigned(2)); err != nil {
				t.Fatal(err)
			}
			if len(f.runs) != 5 {
				t.Fatalf("started %d tasks, want 5", len(f.runs))
			}
			want := func(what string, tags []ecstypes.Tag) {
				t.Helper()
				got := tagMap(tags)
				if got["paddock:repo"] != "acme/gadgets" || got["paddock:pool"] != pool.Name {
					t.Fatalf("%s tagged %v, want paddock:repo acme/gadgets and paddock:pool %s", what, got, pool.Name)
				}
			}
			for i, run := range f.runs {
				want(fmt.Sprintf("task %d", i+1), run.Tags)
				def := f.defs[aws.ToString(run.TaskDefinition)]
				if def == nil {
					t.Fatalf("task %d ran a definition that was not registered", i+1)
				}
				want(fmt.Sprintf("task %d's definition", i+1), def.Tags)
			}
		})
	}
}

// A runner task reads only its own config: its JIT config sits in its own
// parameter, which ECS injects from its own task definition, and the task
// holds no role of its own to read any other.
func TestARunnerTaskGetsOnlyItsOwnConfig(t *testing.T) {
	t.Run("fitness: a runner task reads only its own config and holds no role (runner-isolation.md)", func(t *testing.T) {
		f := newFakeAWS()
		s, jit := newScaler(f, spotTest)
		if err := s.Scale(context.Background(), assigned(2)); err != nil {
			t.Fatal(err)
		}
		if len(f.paramPuts) != 2 {
			t.Fatalf("%d parameters written", len(f.paramPuts))
		}
		for i, run := range f.runs {
			def := f.defs[aws.ToString(run.TaskDefinition)]
			if def.TaskRoleArn != nil {
				t.Errorf("runner task %d holds a role", i)
			}
			if aws.ToString(def.ExecutionRoleArn) != s.Launch.ExecutionRoleARN {
				t.Errorf("execution role %q", aws.ToString(def.ExecutionRoleArn))
			}
			c := def.ContainerDefinitions[0]
			if len(c.Secrets) != 1 || aws.ToString(c.Secrets[0].Name) != "PADDOCK_CONFIG" {
				t.Fatalf("secrets %+v", c.Secrets)
			}
			put := f.paramPuts[i]
			if aws.ToString(c.Secrets[0].ValueFrom) != aws.ToString(put.Name) {
				t.Errorf("task %d reads %q, its config is in %q", i, aws.ToString(c.Secrets[0].ValueFrom), aws.ToString(put.Name))
			}
			if put.Type != ssmtypes.ParameterTypeSecureString {
				t.Errorf("parameter %d is %s", i, put.Type)
			}
			var first struct{ Runner, JITConfig, URL string }
			if err := json.Unmarshal([]byte(aws.ToString(put.Value)), &first); err != nil {
				t.Fatalf("parameter %d: %v", i, err)
			}
			if first.Runner != jit.calls[i] || first.JITConfig != "jit-for-"+jit.calls[i] {
				t.Errorf("parameter %d holds runner %q, config %q", i, first.Runner, first.JITConfig)
			}
			// The URL comes beside the first config, signed once for this
			// task's own object, for a little over the warm limit.
			sign := f.presigned[i]
			if sign.bucket != "configs" || sign.key != "5/"+jit.calls[i] || first.URL != "https://configs.s3.example/5/"+jit.calls[i]+"?X-Amz-Signature=sig" {
				t.Errorf("task %d's URL %q, signed for %+v", i, first.URL, sign)
			}
			if sign.expires <= WarmLimit || sign.expires > WarmLimit+10*time.Minute {
				t.Errorf("URL signed for %s, warm limit %s", sign.expires, WarmLimit)
			}
			if !strings.HasPrefix(aws.ToString(put.Name), "/paddock/runners/5/") {
				t.Errorf("parameter %q", aws.ToString(put.Name))
			}
			for _, e := range c.Environment {
				if strings.Contains(aws.ToString(e.Value), "jit-for-") || strings.Contains(aws.ToString(e.Value), "s3.example") {
					t.Errorf("the JIT config or the URL is in the task's plain environment")
				}
			}
			if aws.ToString(c.Image) != s.Launch.Image {
				t.Errorf("image %q", aws.ToString(c.Image))
			}
		}
		if len(f.deleted) != 2 {
			t.Fatalf("deleted %d of 2 one-off task definitions", len(f.deleted))
		}
		if len(f.presigned) != 2 {
			t.Fatalf("signed %d URLs for 2 tasks", len(f.presigned))
		}
	})
}

func TestAPoolNeverRunsMoreThanItsMaximum(t *testing.T) {
	f := newFakeAWS()
	pool := spotTest
	pool.MaxRunners = 3
	s, _ := newScaler(f, pool)
	if err := s.Scale(context.Background(), assigned(8)); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 3 {
		t.Fatalf("started %d, maximum 3", len(f.runs))
	}
	if err := s.Scale(context.Background(), assigned(8)); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 3 {
		t.Fatalf("started %d more past the maximum", len(f.runs)-3)
	}
}

// A restarted listener counts the tasks already running from ECS and the
// assigned jobs from GitHub, so it starts the difference and none twice.
func TestARestartedListenerStartsOnlyTheMissingRunners(t *testing.T) {
	f := newFakeAWS()
	first, _ := newScaler(f, spotTest)
	if err := first.Scale(context.Background(), assigned(2)); err != nil {
		t.Fatal(err)
	}
	second, _ := newScaler(f, spotTest)
	if err := second.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: -1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 5}}); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 5 {
		t.Fatalf("started %d tasks for 5 jobs", len(f.runs))
	}
}

func TestAFinishedTaskIsReplacedOnlyForAnotherJob(t *testing.T) {
	f := newFakeAWS()
	s, _ := newScaler(f, spotTest)
	ctx := context.Background()
	if err := s.Scale(ctx, assigned(1)); err != nil {
		t.Fatal(err)
	}
	f.stopAll()
	if err := s.Scale(ctx, assigned(0)); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 1 {
		t.Fatalf("started %d tasks for no job", len(f.runs)-1)
	}
	if err := s.Scale(ctx, assigned(1)); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 2 {
		t.Fatalf("started %d tasks for the next job", len(f.runs)-1)
	}
}

func TestAPollWithNoMessageBeforeAnyStatisticsScalesNothing(t *testing.T) {
	f := newFakeAWS()
	s, _ := newScaler(f, spotTest)
	if err := s.Scale(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 0 {
		t.Fatal("a 202 started a task")
	}
}

// An empty poll means GitHub's counts have not changed, so the last ones
// still hold. Scaling on them replaces a task that died before it took its
// job, which no new message would otherwise prompt.
func TestAnEmptyPollReplacesATaskThatDiedOnTheLastCounts(t *testing.T) {
	f := newFakeAWS()
	s, _ := newScaler(f, spotTest)
	ctx := context.Background()
	if err := s.Scale(ctx, assigned(1)); err != nil {
		t.Fatal(err)
	}
	f.stopAll()
	if err := s.Scale(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 2 {
		t.Fatalf("started %d tasks; the dead one was not replaced", len(f.runs))
	}
	if err := s.Scale(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 2 {
		t.Fatal("an empty poll started a task the pool already has")
	}
}

// A refused start must not end the message session: a pool at its Fargate
// quota would otherwise reopen its session every few seconds and read no
// messages until the quota frees. The start is logged, and tried again on a
// later scale.
func TestARefusedStartIsLoggedAndRetriedAndKeepsTheSession(t *testing.T) {
	for _, refusal := range []struct {
		name   string
		refuse func(*fakeAWS)
		reason string
	}{
		{"failures", func(f *fakeAWS) { f.failRun = true }, "RESOURCE:FARGATE"},
		{"error", func(f *fakeAWS) {
			f.runErr = errors.New("You've reached the limit on the number of vCPUs you can run as spot tasks concurrently")
		}, "limit on the number of vCPUs"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			f := newFakeAWS()
			refusal.refuse(f)
			s, _ := newScaler(f, spotTest)
			l := withLogs(s)
			ctx := context.Background()
			if err := s.Scale(ctx, assigned(3)); err != nil {
				t.Fatalf("a refused start ended the session: %v", err)
			}
			if len(f.runs) != 1 {
				t.Fatalf("asked ECS %d times after it refused; once a scale is enough", len(f.runs))
			}
			refused := l.lines(logStartRefused)
			if len(refused) != 1 || !strings.Contains(fmt.Sprint(refused[0]["reason"]), refusal.reason) {
				t.Fatalf("the refusal was not logged with its reason: %v", refused)
			}
			if len(f.params) != 0 {
				t.Fatalf("left %d configs behind", len(f.params))
			}
			if len(f.deleted) != 1 {
				t.Fatal("left the one-off task definition behind")
			}

			f.failRun, f.runErr = false, nil
			if err := s.Scale(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if len(f.tasks) != 3 {
				t.Fatalf("the next scale started %d of the 3 tasks refused", len(f.tasks))
			}
		})
	}
}

// A config is needed only until ECS has injected it into its task. Once the
// task runs, or is gone, the parameter is deleted.
func TestAConfigIsDeletedOnceItsTaskRuns(t *testing.T) {
	f := newFakeAWS()
	s, jit := newScaler(f, spotTest)
	s.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	ctx := context.Background()
	if err := s.Scale(ctx, assigned(1)); err != nil {
		t.Fatal(err)
	}
	if len(f.params) != 1 {
		t.Fatal("the pending task's config is gone")
	}
	f.setLastStatus("RUNNING")
	// Its runner took its job: ten minutes on, a task that had not would be
	// a failed start, and replaced.
	if err := s.Scale(ctx, started(1, jit.calls[0])); err != nil {
		t.Fatal(err)
	}
	if len(f.params) != 0 {
		t.Fatalf("%d configs remain once their tasks run", len(f.params))
	}
}

// ListTasks can lag a RunTask, so a config just written is kept a while.
func TestAFreshConfigIsKeptThoughNoTaskShowsYet(t *testing.T) {
	f := newFakeAWS()
	s, _ := newScaler(f, spotTest)
	ctx := context.Background()
	if err := s.Scale(ctx, assigned(1)); err != nil {
		t.Fatal(err)
	}
	f.tasks = nil
	if err := s.Scale(ctx, assigned(0)); err != nil {
		t.Fatal(err)
	}
	if len(f.params) != 1 {
		t.Fatal("a config written a moment ago was deleted")
	}
}

// The listener sets a pool's settings when the list changes; the next task
// the scaler starts takes them, and the tasks already running keep theirs.
func TestAChangedPoolSettingAppliesToTheNextTaskStarted(t *testing.T) {
	f := newFakeAWS()
	s, _ := newScaler(f, spotTest)
	s.Settings = repos.NewCurrent(spotTest)
	if err := s.Scale(context.Background(), assigned(1)); err != nil {
		t.Fatal(err)
	}
	s.Settings.Set(repos.Pool{Name: "test", Capacity: repos.OnDemand, Arch: repos.ARM64, VCPU: 4, MemoryGB: 16, MaxRunners: 2, MaxJob: time.Hour})
	if err := s.Scale(context.Background(), assigned(5)); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 2 {
		t.Fatalf("started %d tasks, the new maximum is 2", len(f.runs))
	}
	if got := aws.ToString(f.runs[0].CapacityProviderStrategy[0].CapacityProvider); got != "FARGATE_SPOT" {
		t.Errorf("the first task's capacity %s", got)
	}
	run := f.runs[1]
	if got := aws.ToString(run.CapacityProviderStrategy[0].CapacityProvider); got != "FARGATE" {
		t.Errorf("the next task's capacity %s", got)
	}
	if def := f.defs[aws.ToString(run.TaskDefinition)]; aws.ToString(def.Cpu) != "4096" || aws.ToString(def.Memory) != "16384" {
		t.Errorf("the next task's size %s/%s", aws.ToString(def.Cpu), aws.ToString(def.Memory))
	}
	if first := f.defs[aws.ToString(f.runs[0].TaskDefinition)]; first.RuntimePlatform.CpuArchitecture != ecstypes.CPUArchitectureX8664 {
		t.Errorf("the first task's architecture %q", first.RuntimePlatform.CpuArchitecture)
	}
	if def := f.defs[aws.ToString(run.TaskDefinition)]; def.RuntimePlatform.CpuArchitecture != ecstypes.CPUArchitectureArm64 {
		t.Errorf("the next task's architecture %q", def.RuntimePlatform.CpuArchitecture)
	}
	if s.Pool.MaxJob != time.Hour {
		t.Errorf("the pool's maximum job time %s", s.Pool.MaxJob)
	}
}

// A removed pool's scale set goes once no task of it runs, so the listener
// counts a scale set's live tasks the way the scaler finds them.
func TestLiveTasksCountsAScaleSetsRunningTasks(t *testing.T) {
	f := newFakeAWS()
	s, _ := newScaler(f, spotTest)
	if err := s.Scale(context.Background(), assigned(2)); err != nil {
		t.Fatal(err)
	}
	n, err := LiveTasks(context.Background(), f, "paddock", 5)
	if err != nil || n != 2 {
		t.Fatalf("%d live tasks, %v", n, err)
	}
	if n, _ := LiveTasks(context.Background(), f, "paddock", 6); n != 0 {
		t.Fatalf("another scale set has %d live tasks", n)
	}
	f.stopAll()
	if n, _ := LiveTasks(context.Background(), f, "paddock", 5); n != 0 {
		t.Fatalf("%d live tasks after all stopped", n)
	}
}
