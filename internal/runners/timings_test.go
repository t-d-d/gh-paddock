package runners

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// setTimes gives every task ECS's timestamps for its start: created at
// created, the pull from pull to pulled, and the container started at
// running. A zero time leaves that timestamp unset. Each task's container
// carries its image, as ECS describes it, so a line that named the image
// would show.
func (f *fakeAWS) setTimes(created, pull, pulled, running time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		return aws.Time(t)
	}
	for i := range f.tasks {
		f.tasks[i].CreatedAt = at(created)
		f.tasks[i].PullStartedAt = at(pull)
		f.tasks[i].PullStoppedAt = at(pulled)
		f.tasks[i].StartedAt = at(running)
		f.tasks[i].Containers = []ecstypes.Container{{
			Name:        aws.String("runner"),
			Image:       aws.String("registry.example/runner@sha256:abc"),
			ImageDigest: aws.String("sha256:abc"),
		}}
	}
}

func TestEachRunnerTaskLogsItsStartTimesOnce(t *testing.T) {
	f := newFakeAWS()
	s, jit := newScaler(f, spotTest)
	l := withLogs(s)
	mustScale(t, s, assigned(1))

	created := time.Now().Truncate(time.Second)
	pull := created.Add(12500 * time.Millisecond)
	pulled := pull.Add(30 * time.Second)
	running := pulled.Add(4200 * time.Millisecond)

	// Placed and pulling: the container has not started.
	f.setTimes(created, pull, time.Time{}, time.Time{})
	f.setLastStatus("PENDING")
	mustScale(t, s, nil)
	if got := l.lines(logTaskTimes); len(got) != 0 {
		t.Fatalf("logged %v before the container started", got)
	}

	f.setTimes(created, pull, pulled, running)
	f.setLastStatus("RUNNING")
	mustScale(t, s, nil)
	mustScale(t, s, nil)

	got := l.lines(logTaskTimes)
	if len(got) != 1 {
		t.Fatalf("logged %d lines of the task's times, want 1: %v", len(got), got)
	}
	line := got[0]
	arn := aws.ToString(f.tasks[0].TaskArn)
	if line["task"] != shortARN(arn) {
		t.Errorf("task = %v, want the task's id %s", line["task"], shortARN(arn))
	}
	if line["runner"] != jit.calls[0] {
		t.Errorf("runner = %v, want its first runner %s", line["runner"], jit.calls[0])
	}
	for key, want := range map[string]float64{"placementSeconds": 12.5, "pullSeconds": 30, "startSeconds": 4.2} {
		if line[key] != want {
			t.Errorf("%s = %v, want %v", key, line[key], want)
		}
	}

	t.Run("fitness: the times name the task by its id, never an ARN or a registry (public-repo.md)", func(t *testing.T) {
		raw := l.buf.String()
		for _, line := range strings.Split(raw, "\n") {
			if !strings.Contains(line, logTaskTimes) {
				continue
			}
			if strings.Contains(line, "arn:") || strings.Contains(line, "123456789012") || strings.Contains(line, "registry.example") || strings.Contains(line, "sha256") {
				t.Errorf("the line names an ARN or an image: %s", line)
			}
		}
	})
}

func TestATaskThatNeverStartsItsContainerLogsNoTimes(t *testing.T) {
	f := newFakeAWS()
	s, _ := newScaler(f, spotTest)
	l := withLogs(s)
	mustScale(t, s, assigned(1))

	created := time.Now().Truncate(time.Second)
	f.setTimes(created, created.Add(10*time.Second), time.Time{}, time.Time{})
	f.setLastStatus("PENDING")
	mustScale(t, s, nil)
	f.stopAll()
	mustScale(t, s, nil)

	if got := l.lines(logTaskTimes); len(got) != 0 {
		t.Errorf("logged %v for a task whose container never started", got)
	}
}

func TestARestartedListenerMayLogALiveTasksTimesAgain(t *testing.T) {
	f := newFakeAWS()
	s, _ := newScaler(f, spotTest)
	l := withLogs(s)
	mustScale(t, s, assigned(1))

	created := time.Now().Truncate(time.Second)
	f.setTimes(created, created.Add(10*time.Second), created.Add(40*time.Second), created.Add(45*time.Second))
	f.setLastStatus("RUNNING")
	mustScale(t, s, nil)

	// A new listener knows nothing of what the last one logged.
	restarted, _ := newScaler(f, spotTest)
	restarted.Log = s.Log
	mustScale(t, restarted, assigned(1))

	if got := l.lines(logTaskTimes); len(got) != 2 {
		t.Errorf("logged %d lines of the task's times across a restart, want 2", len(got))
	}
}
