// Package runners starts a pool's runner tasks: one single-use runner per
// task, on the pool's capacity and size, to match the jobs GitHub has
// assigned to the pool's scale set.
//
// Each runner's just-in-time config goes in its own SecureString parameter,
// and the runner's task definition, registered for that one task, names
// that parameter alone. ECS injects it as the runner starts; the task holds
// no role, so it can read nothing else
// (docs/constraints/runner-isolation.md).
package runners

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
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

// ECS is the part of the ECS client the scaler calls.
type ECS interface {
	ListTasks(context.Context, *ecs.ListTasksInput, ...func(*ecs.Options)) (*ecs.ListTasksOutput, error)
	DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	RegisterTaskDefinition(context.Context, *ecs.RegisterTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error)
	DeregisterTaskDefinition(context.Context, *ecs.DeregisterTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.DeregisterTaskDefinitionOutput, error)
	DeleteTaskDefinitions(context.Context, *ecs.DeleteTaskDefinitionsInput, ...func(*ecs.Options)) (*ecs.DeleteTaskDefinitionsOutput, error)
	RunTask(context.Context, *ecs.RunTaskInput, ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
}

// SSM is the part of the Parameter Store client the scaler calls.
type SSM interface {
	PutParameter(context.Context, *ssm.PutParameterInput, ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	DeleteParameter(context.Context, *ssm.DeleteParameterInput, ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
	GetParametersByPath(context.Context, *ssm.GetParametersByPathInput, ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
}

// JIT makes a runner's just-in-time config.
type JIT interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
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
}

// Scaler keeps one pool's runner tasks matched to its assigned jobs.
type Scaler struct {
	ECS        ECS
	SSM        SSM
	JIT        JIT
	Launch     Launch
	Repo       repos.Repo
	Pool       repos.Pool
	ScaleSetID int
	Log        *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

func (s *Scaler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scaler) startedBy() string { return "paddock-" + strconv.Itoa(s.ScaleSetID) }

func (s *Scaler) configDir() string { return ConfigPath + "/" + strconv.Itoa(s.ScaleSetID) }

// Scale starts a runner task for each assigned job that has none, up to the
// pool's maximum. GitHub's statistics count every job assigned to the set,
// running or waiting, and ECS counts every task that has not stopped, so a
// listener that restarts starts only what is missing. A poll that carried
// no message changes nothing.
func (s *Scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil || msg.Statistics == nil {
		return nil
	}
	tasks, err := s.liveTasks(ctx)
	if err != nil {
		return err
	}
	if err := s.sweepConfigs(ctx, tasks); err != nil {
		s.Log.Warn("could not clear runner configs", "error", err.Error())
	}
	want := min(msg.Statistics.TotalAssignedJobs, s.Pool.MaxRunners)
	for have := len(tasks); have < want; have++ {
		if err := s.start(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scaler) liveTasks(ctx context.Context) ([]ecstypes.Task, error) {
	var arns []string
	var next *string
	for {
		out, err := s.ECS.ListTasks(ctx, &ecs.ListTasksInput{
			Cluster:       aws.String(s.Launch.Cluster),
			StartedBy:     aws.String(s.startedBy()),
			DesiredStatus: ecstypes.DesiredStatusRunning,
			NextToken:     next,
		})
		if err != nil {
			return nil, fmt.Errorf("list the pool's tasks: %w", err)
		}
		arns = append(arns, out.TaskArns...)
		if next = out.NextToken; next == nil {
			break
		}
	}
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
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	runner := "paddock-" + hex.EncodeToString(suffix)
	jit, err := s.JIT.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: runner, WorkFolder: "_work"}, s.ScaleSetID)
	if err != nil {
		return fmt.Errorf("make a runner's config: %w", err)
	}
	param := s.configDir() + "/" + runner
	if _, err := s.SSM.PutParameter(ctx, &ssm.PutParameterInput{
		Name:  aws.String(param),
		Value: aws.String(jit.EncodedJITConfig),
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
	def, err := s.ECS.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family:                  aws.String(TaskFamily),
		RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
		NetworkMode:             ecstypes.NetworkModeAwsvpc,
		Cpu:                     aws.String(strconv.Itoa(s.Pool.VCPU * 1024)),
		Memory:                  aws.String(strconv.Itoa(s.Pool.MemoryGB * 1024)),
		RuntimePlatform: &ecstypes.RuntimePlatform{
			CpuArchitecture:       ecstypes.CPUArchitectureX8664,
			OperatingSystemFamily: ecstypes.OSFamilyLinux,
		},
		ExecutionRoleArn: aws.String(s.Launch.ExecutionRoleARN),
		ContainerDefinitions: []ecstypes.ContainerDefinition{{
			Name:      aws.String("runner"),
			Image:     aws.String(s.Launch.Image),
			Essential: aws.Bool(true),
			Secrets:   []ecstypes.Secret{{Name: aws.String("ACTIONS_RUNNER_INPUT_JITCONFIG"), ValueFrom: aws.String(param)}},
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
		return fmt.Errorf("start a runner task: %w", err)
	}
	if len(out.Tasks) == 0 {
		reason := "no reason given"
		if len(out.Failures) > 0 {
			reason = aws.ToString(out.Failures[0].Reason)
		}
		return fmt.Errorf("ECS started no runner task: %s", reason)
	}
	started = true
	arn := aws.ToString(out.Tasks[0].TaskArn)
	s.Log.Info("runner task started", "repo", s.Repo.FullName(), "pool", s.Pool.Name, "runner", runner,
		"task", arn[strings.LastIndex(arn, "/")+1:], "capacity", provider)
	return nil
}
