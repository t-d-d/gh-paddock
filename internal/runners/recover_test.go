package runners

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/t-d-d/gh-paddock/internal/github"
)

// logs is a scaler's log, as JSON lines the test reads back.
type logs struct{ buf bytes.Buffer }

func withLogs(s *Scaler) *logs {
	l := &logs{}
	s.Log = slog.New(slog.NewJSONHandler(&l.buf, nil)).With("repo", s.Repo.FullName(), "pool", s.Pool.Name)
	return l
}

func (l *logs) lines(msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(l.buf.String(), "\n") {
		var v map[string]any
		if json.Unmarshal([]byte(line), &v) == nil && v["msg"] == msg {
			out = append(out, v)
		}
	}
	return out
}

// fakeActions is GitHub's Actions API for acme/widgets, in memory: each run's
// status and attempts, and the jobs re-run.
type fakeActions struct {
	runs   map[int64]*fakeRun
	reruns []int64
	// gone are runs deleted from GitHub, which it answers 404.
	gone  map[int64]bool
	reads int
	// listed counts the reads of the runs in progress.
	listed int
	// listFails is how many reads of the runs in progress fail first.
	listFails int
}

type fakeRun struct {
	status   string
	attempts [][]github.Job
}

func (a *fakeActions) Run(_ context.Context, owner, name string, id int64) (github.Run, error) {
	a.reads++
	if a.gone[id] {
		return github.Run{}, &github.StatusError{Method: "GET", Path: fmt.Sprintf("/repos/%s/%s/actions/runs/%d", owner, name, id), Status: 404, Message: "Not Found"}
	}
	r, ok := a.runs[id]
	if !ok || owner != "acme" || name != "widgets" {
		return github.Run{}, fmt.Errorf("no run %s/%s %d", owner, name, id)
	}
	return github.Run{Status: r.status, Attempt: len(r.attempts)}, nil
}

func (a *fakeActions) RunsInProgress(_ context.Context, owner, name string) ([]github.Run, error) {
	a.listed++
	if a.listFails > 0 {
		a.listFails--
		return nil, errors.New("GitHub answered 502")
	}
	if owner != "acme" || name != "widgets" {
		return nil, fmt.Errorf("listed the runs of %s/%s", owner, name)
	}
	var out []github.Run
	for id, r := range a.runs {
		if r.status == "in_progress" {
			out = append(out, github.Run{ID: id, Status: r.status, Attempt: len(r.attempts)})
		}
	}
	return out, nil
}

func (a *fakeActions) RunJobs(_ context.Context, _, _ string, id int64, attempt int) ([]github.Job, error) {
	return a.runs[id].attempts[attempt-1], nil
}

// RerunJob starts a new attempt in which the job runs on a new runner.
func (a *fakeActions) RerunJob(_ context.Context, _, _ string, jobID int64) error {
	a.reruns = append(a.reruns, jobID)
	for _, r := range a.runs {
		last := r.attempts[len(r.attempts)-1]
		for _, j := range last {
			if j.ID == jobID {
				r.status = "queued"
				r.attempts = append(r.attempts, []github.Job{{ID: jobID + 1000, RunnerName: "paddock-rerun"}})
				return nil
			}
		}
	}
	return errors.New("no such job")
}

// fakeMetrics answers a pool's 30-day sums.
type fakeMetrics struct {
	sums  map[string]float64
	asked []*cloudwatch.GetMetricDataInput
}

func (m *fakeMetrics) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	m.asked = append(m.asked, in)
	var out cloudwatch.GetMetricDataOutput
	for _, q := range in.MetricDataQueries {
		var values []float64
		if v, ok := m.sums[aws.ToString(q.MetricStat.Metric.MetricName)]; ok {
			values = []float64{v}
		}
		out.MetricDataResults = append(out.MetricDataResults, cwtypes.MetricDataResult{Id: q.Id, Values: values})
	}
	return &out, nil
}

func recovering(t *testing.T) (*fakeAWS, *Scaler, *fakeJIT, *clock, *logs) {
	t.Helper()
	f := newFakeAWS()
	s, jit := newScaler(f, spotTest)
	jit.aws = f
	c := withClock(s)
	l := withLogs(s)
	return f, s, jit, c, l
}

func startedRun(assignedJobs int, runner string, runID int64) *scaleset.RunnerScaleSetMessage {
	msg := assigned(assignedJobs)
	msg.JobStartedMessages = append(msg.JobStartedMessages, &scaleset.JobStarted{RunnerName: runner, JobMessageBase: scaleset.JobMessageBase{WorkflowRunID: runID}})
	return msg
}

// GitHub refuses to remove a runner whose job is running: the runner took
// its job, though no message said so, so it is asked no more.
func TestARunnerGitHubSaysIsRunningAJobIsNotAskedToBeRemovedAgain(t *testing.T) {
	f, s, jit, c, _ := recovering(t)
	jit.jobRunning = true
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	c.add(FailedStartAfter + time.Second)
	mustScale(t, s, nil)
	c.add(time.Minute)
	mustScale(t, s, nil)
	if jit.removeTries != 1 || len(f.stopped) != 0 {
		t.Fatalf("tried %d removals, stopped %v", jit.removeTries, f.stopped)
	}
}

func TestAStartWhoseRunnerNeverTakesItsJobIsReplaced(t *testing.T) {
	t.Run("fitness: the listener stops only a task whose runner never took its job, once its runner is removed (remove-then-terminate.md)", func(t *testing.T) {
		f, s, jit, c, l := recovering(t)
		mustScale(t, s, assigned(1))
		f.setLastStatus("RUNNING")
		first := jit.calls[0]

		c.add(FailedStartAfter - time.Second)
		mustScale(t, s, nil)
		if len(f.stopped) != 0 {
			t.Fatal("stopped a task before its runner had three minutes")
		}
		c.add(2 * time.Second)
		mustScale(t, s, nil)
		want := []string{"remove " + first, "stop " + first}
		if strings.Join(f.events, ", ") != strings.Join(want, ", ") {
			t.Fatalf("events %v, want %v", f.events, want)
		}
		if len(f.runs) != 2 {
			t.Fatalf("started %d replacements", len(f.runs)-1)
		}
		if got := l.lines("failed start"); len(got) != 1 || got[0]["runner"] != first || got[0]["inARow"] != 1.0 {
			t.Fatalf("logged %v", got)
		}
	})
}

func TestATaskWhoseRunnerTookItsJobIsNeverStopped(t *testing.T) {
	t.Run("fitness: the listener stops only a task whose runner never took its job, once its runner is removed (remove-then-terminate.md)", func(t *testing.T) {
		f, s, jit, c, _ := recovering(t)
		mustScale(t, s, assigned(1))
		f.setLastStatus("RUNNING")
		mustScale(t, s, startedRun(1, jit.calls[0], 5))
		c.add(3 * time.Hour)
		mustScale(t, s, nil)
		if len(f.stopped) != 0 || len(jit.removed) != 0 {
			t.Fatalf("stopped %v, removed %v", f.stopped, jit.removed)
		}
	})
}

func TestARunnerThatCannotBeRemovedKeepsItsTask(t *testing.T) {
	t.Run("fitness: the listener stops only a task whose runner never took its job, once its runner is removed (remove-then-terminate.md)", func(t *testing.T) {
		f, s, jit, c, _ := recovering(t)
		jit.refuseRemove = true
		mustScale(t, s, assigned(1))
		f.setLastStatus("RUNNING")
		c.add(FailedStartAfter + time.Second)
		mustScale(t, s, nil)
		if len(f.stopped) != 0 {
			t.Fatal("stopped a task whose runner GitHub would not remove")
		}
	})
}

func TestAConfigHandedToAWaitingTaskThatNeverStartsIsAFailedStart(t *testing.T) {
	f, s, jit, c, _ := recovering(t)
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	first := jit.calls[0]
	mustScale(t, s, startedRun(1, first, 5))
	c.add(10 * time.Minute)
	mustScale(t, s, finished(1, first))
	if len(jit.calls) != 2 {
		t.Fatal("the waiting task was handed no config")
	}
	c.add(FailedStartAfter + time.Second)
	mustScale(t, s, nil)
	if len(jit.removed) != 1 || jit.removed[0] != jit.calls[1] || len(f.stopped) != 1 {
		t.Fatalf("removed %v, stopped %v", jit.removed, f.stopped)
	}
	if len(f.runs) != 2 {
		t.Fatal("the job got no new task")
	}
}

func TestFailedStartsAreCountedInARowUntilAJobStarts(t *testing.T) {
	f, s, jit, c, l := recovering(t)
	mustScale(t, s, assigned(1))
	for range 3 {
		f.setLastStatus("RUNNING")
		c.add(FailedStartAfter + time.Second)
		mustScale(t, s, nil)
	}
	got := l.lines("failed start")
	if len(got) != 3 || got[2]["inARow"] != 3.0 {
		t.Fatalf("logged %v", got)
	}
	f.setLastStatus("RUNNING")
	mustScale(t, s, startedRun(1, jit.calls[len(jit.calls)-1], 5))
	if s.failedInARow != 0 {
		t.Fatalf("%d failed starts in a row after a job started", s.failedInARow)
	}
}

// A job's start is written to its task's tags, so a listener that restarts
// still knows which tasks' runners took their jobs.
func TestARestartedListenerStopsNoTaskWhoseRunnerTookItsJob(t *testing.T) {
	f, s, jit, c, _ := recovering(t)
	mustScale(t, s, assigned(2))
	f.setLastStatus("RUNNING")
	mustScale(t, s, startedRun(2, jit.calls[0], 5))
	if got := tagOf(f.tasks[0], "paddock:job"); got != jit.calls[0]+"@5" {
		t.Fatalf("job tag %q", got)
	}

	next, _ := newScaler(f, spotTest)
	next.JIT = jit
	next.Now = c.now
	c.add(FailedStartAfter + time.Second)
	mustScale(t, next, nil)
	if len(f.stopped) != 1 || !strings.HasSuffix(f.stopped[0], aws.ToString(f.tasks[1].TaskArn)) {
		t.Fatalf("stopped %v, want the second task alone", f.stopped)
	}
}

// reclaim has Spot reclaim the live task i now: ECS marks it stopping, and
// it stops at the end of the two minutes' warning.
func reclaim(f *fakeAWS, i int, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[i].DesiredStatus = aws.String("STOPPED")
	f.tasks[i].LastStatus = aws.String("STOPPED")
	f.tasks[i].StopCode = ecstypes.TaskStopCodeSpotInterruption
	f.tasks[i].StoppingAt = aws.Time(at)
	f.tasks[i].StoppedAt = aws.Time(at.Add(2 * time.Minute))
}

// A runner can take its job with no message heard, as when the listener's
// session keeps ending. GitHub's refusal to remove the runner says it took
// one, so the listener finds the job's run and tags the task with it, and a
// Spot reclaim that kills the job gets it re-run once.
func TestAReclaimedJobWhoseStartWasNotHeardIsReRunOnce(t *testing.T) {
	f, s, jit, c, l := recovering(t)
	actions := &fakeActions{runs: map[int64]*fakeRun{}}
	s.Actions = actions
	jit.jobRunning = true
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	runner := jit.calls[0]
	actions.runs[9] = &fakeRun{status: "in_progress", attempts: [][]github.Job{{
		{ID: 90, RunnerName: "paddock-other"},
		{ID: 91, RunnerName: runner},
	}}}
	c.add(FailedStartAfter + time.Second)
	mustScale(t, s, nil)
	if got := tagOf(f.tasks[0], "paddock:job"); got != runner+"@9" {
		t.Fatalf("job tag %q, want %q", got, runner+"@9")
	}
	if len(l.lines("found the run of a job no message named")) != 1 {
		t.Fatalf("log %s", l.buf.String())
	}

	at := c.now()
	reclaim(f, 0, at)
	actions.runs[9].status = "completed"
	actions.runs[9].attempts[0][1] = github.Job{ID: 91, RunnerName: runner, Conclusion: "failure", CompletedAt: at.Add(5 * time.Minute)}
	for range 3 {
		c.add(3 * time.Minute)
		mustScale(t, s, nil)
	}
	if len(actions.reruns) != 1 || actions.reruns[0] != 91 {
		t.Fatalf("re-ran %v, want job 91 once", actions.reruns)
	}
}

// A task's tag can name an earlier runner's job. A later runner found
// running a job no message named takes the tag over, once its run shows in
// GitHub's list, which is read again a minute on.
func TestALaterRunnersUnheardJobReplacesTheTaskTagOnceItsRunIsFound(t *testing.T) {
	f, s, jit, c, _ := recovering(t)
	actions := &fakeActions{runs: map[int64]*fakeRun{}}
	s.Actions = actions
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	first := jit.calls[0]
	mustScale(t, s, startedRun(1, first, 5))
	c.add(10 * time.Minute)
	mustScale(t, s, finished(1, first))
	second := jit.calls[1]
	jit.jobRunning = true
	c.add(FailedStartAfter + time.Second)
	mustScale(t, s, nil)
	if got := tagOf(f.tasks[0], "paddock:job"); got != first+"@5" {
		t.Fatalf("job tag %q before the run shows", got)
	}
	actions.runs[9] = &fakeRun{status: "in_progress", attempts: [][]github.Job{{{ID: 91, RunnerName: second}}}}
	mustScale(t, s, nil)
	if actions.listed != 1 {
		t.Fatalf("listed the runs %d times inside a minute", actions.listed)
	}
	c.add(time.Minute)
	mustScale(t, s, nil)
	if got := tagOf(f.tasks[0], "paddock:job"); got != second+"@9" {
		t.Fatalf("job tag %q, want %q", got, second+"@9")
	}
	c.add(time.Minute)
	mustScale(t, s, nil)
	if actions.listed != 2 {
		t.Fatalf("listed the runs %d times, after the run was found", actions.listed)
	}
}

// A runner whose unheard job's run was never found can finish, heard, and
// its task be handed a new runner. Nothing says the new runner runs a job
// yet, so the runs in progress are not read for it.
func TestATaskHandedANewRunnerSeeksNoRunForTheLastOne(t *testing.T) {
	f, s, jit, c, _ := recovering(t)
	actions := &fakeActions{runs: map[int64]*fakeRun{}}
	s.Actions = actions
	jit.jobRunning = true
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	first := jit.calls[0]
	c.add(FailedStartAfter + time.Second)
	mustScale(t, s, nil)
	jit.jobRunning = false
	mustScale(t, s, finished(1, first))
	if len(jit.calls) != 2 {
		t.Fatalf("handed %d runners, want a second", len(jit.calls))
	}
	listed := actions.listed
	c.add(time.Minute)
	mustScale(t, s, nil)
	if actions.listed != listed {
		t.Fatalf("listed the runs in progress %d more times for a runner with no job", actions.listed-listed)
	}
}

// The job runs when the search begins, so its run is in progress then. A
// run not found in five searches is logged and sought no more, which bounds
// what the search costs.
func TestARunNotFoundInFiveSearchesIsLoggedAndSoughtNoMore(t *testing.T) {
	f, s, jit, c, l := recovering(t)
	actions := &fakeActions{runs: map[int64]*fakeRun{}}
	s.Actions = actions
	jit.jobRunning = true
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	c.add(FailedStartAfter + time.Second)
	for range 8 {
		mustScale(t, s, nil)
		c.add(time.Minute)
	}
	if actions.listed != 5 {
		t.Fatalf("listed the runs in progress %d times, want 5", actions.listed)
	}
	if got := l.lines("could not find the run of a job no message named"); len(got) != 1 || got[0]["runner"] != jit.calls[0] {
		t.Fatalf("logged %v", got)
	}
}

// A task Spot reclaims inside the three minutes after its runner's config is
// handed over leaves the live list before GitHub is asked whether its runner
// is busy. With no start message heard, the reclaim check finds the runner's
// job among the runs in progress, and re-runs it once.
func TestAJobReclaimedBeforeItsStartIsKnownIsReRunOnce(t *testing.T) {
	f, s, jit, c, l := recovering(t)
	actions := &fakeActions{runs: map[int64]*fakeRun{}}
	s.Actions = actions
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	runner := jit.calls[0]
	actions.runs[9] = &fakeRun{status: "in_progress", attempts: [][]github.Job{{
		{ID: 90, RunnerName: "paddock-other"},
		{ID: 91, RunnerName: runner},
	}}}
	c.add(2 * time.Minute)
	at := c.now()
	reclaim(f, 0, at)
	c.add(3 * time.Minute)
	mustScale(t, s, assigned(0))
	if len(jit.removed) != 0 || len(f.stopped) != 0 {
		t.Fatalf("removed %v, stopped %v", jit.removed, f.stopped)
	}
	actions.runs[9].status = "completed"
	actions.runs[9].attempts[0][1] = github.Job{ID: 91, RunnerName: runner, Conclusion: "failure", CompletedAt: at.Add(6 * time.Minute)}
	for range 3 {
		c.add(time.Minute)
		mustScale(t, s, nil)
	}
	if len(actions.reruns) != 1 || actions.reruns[0] != 91 {
		t.Fatalf("re-ran %v, want job 91 once", actions.reruns)
	}
	if len(l.lines("found the run of a job no message named")) != 1 {
		t.Fatalf("log %s", l.buf.String())
	}
}

// A runner found busy with an unheard job, whose first search for its run
// fails, can be reclaimed before the next search. The reclaim check finds
// the run instead, and the job is re-run once.
func TestAJobReclaimedAfterAFailedFirstSearchIsReRunOnce(t *testing.T) {
	f, s, jit, c, _ := recovering(t)
	actions := &fakeActions{runs: map[int64]*fakeRun{}, listFails: 1}
	s.Actions = actions
	jit.jobRunning = true
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	runner := jit.calls[0]
	actions.runs[9] = &fakeRun{status: "in_progress", attempts: [][]github.Job{{{ID: 91, RunnerName: runner}}}}
	c.add(FailedStartAfter + time.Second)
	mustScale(t, s, nil)
	if got := tagOf(f.tasks[0], "paddock:job"); got != "" {
		t.Fatalf("job tag %q after a failed search", got)
	}
	at := c.now().Add(10 * time.Second)
	reclaim(f, 0, at)
	c.add(3 * time.Minute)
	mustScale(t, s, nil)
	actions.runs[9].status = "completed"
	actions.runs[9].attempts[0][0] = github.Job{ID: 91, RunnerName: runner, Conclusion: "failure", CompletedAt: at.Add(6 * time.Minute)}
	for range 3 {
		c.add(time.Minute)
		mustScale(t, s, nil)
	}
	if len(actions.reruns) != 1 || actions.reruns[0] != 91 {
		t.Fatalf("re-ran %v, want job 91 once", actions.reruns)
	}
}

// A task's tag can name an earlier runner's job when Spot reclaims it under
// a later one. The job sought is the later runner's.
func TestAReclaimedTaskTaggedWithAnEarlierRunnersJobGetsItsLastRunnersJobReRun(t *testing.T) {
	f, s, jit, c, _ := recovering(t)
	actions := &fakeActions{runs: map[int64]*fakeRun{}}
	s.Actions = actions
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	first := jit.calls[0]
	mustScale(t, s, startedRun(1, first, 5))
	c.add(10 * time.Minute)
	mustScale(t, s, finished(1, first))
	second := jit.calls[1]
	actions.runs[5] = &fakeRun{status: "completed", attempts: [][]github.Job{{{ID: 51, RunnerName: first, Conclusion: "success", CompletedAt: c.now()}}}}
	actions.runs[9] = &fakeRun{status: "in_progress", attempts: [][]github.Job{{{ID: 91, RunnerName: second}}}}
	c.add(time.Minute)
	at := c.now()
	reclaim(f, 0, at)
	c.add(3 * time.Minute)
	mustScale(t, s, nil)
	actions.runs[9].status = "completed"
	actions.runs[9].attempts[0][0] = github.Job{ID: 91, RunnerName: second, Conclusion: "failure", CompletedAt: at.Add(6 * time.Minute)}
	for range 3 {
		c.add(time.Minute)
		mustScale(t, s, nil)
	}
	if len(actions.reruns) != 1 || actions.reruns[0] != 91 {
		t.Fatalf("re-ran %v, want job 91 once", actions.reruns)
	}
}

// The tag that names a task's last runner is written after its config, and
// can fail. A reclaimed task whose job tag names a later runner than its
// handed tag still gets that job re-run.
func TestAReclaimedTaskWhoseHandedTagWasNotWrittenStillGetsItsTaggedJobReRun(t *testing.T) {
	f, s, _, c, _ := recovering(t)
	actions := &fakeActions{runs: map[int64]*fakeRun{5: {status: "in_progress", attempts: [][]github.Job{{}}}}}
	s.Actions = actions
	at := c.now().Add(-time.Minute)
	spotKilled(f, "paddock-killed", at)
	f.mu.Lock()
	var tags []ecstypes.Tag
	for _, tg := range f.tasks[len(f.tasks)-1].Tags {
		if aws.ToString(tg.Key) != "paddock:handed" {
			tags = append(tags, tg)
		}
	}
	f.tasks[len(f.tasks)-1].Tags = tags
	f.mu.Unlock()
	actions.runs[5].attempts[0] = []github.Job{{ID: 72, RunnerName: "paddock-killed", Conclusion: "failure", CompletedAt: at.Add(3 * time.Minute)}}
	actions.runs[5].status = "completed"
	mustScale(t, s, assigned(0))
	if len(actions.reruns) != 1 || actions.reruns[0] != 72 {
		t.Fatalf("re-ran %v, want job 72 once", actions.reruns)
	}
}

// A task reclaimed while its last runner had no job owes nothing: the runs
// in progress are read a few times, then no more.
func TestAReclaimedTaskWhoseLastRunnerHadNoJobIsSoughtFiveTimes(t *testing.T) {
	f, s, _, c, _ := recovering(t)
	actions := &fakeActions{runs: map[int64]*fakeRun{}}
	s.Actions = actions
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	c.add(time.Minute)
	reclaim(f, 0, c.now())
	c.add(3 * time.Minute)
	for range 8 {
		mustScale(t, s, nil)
		c.add(time.Minute)
	}
	if actions.listed != 5 || len(actions.reruns) != 0 {
		t.Fatalf("listed the runs %d times, re-ran %v", actions.listed, actions.reruns)
	}
}

// A start message that comes late still names the run.
func TestALateStartMessageTagsAJobWhoseRunWasNotFound(t *testing.T) {
	f, s, jit, c, _ := recovering(t)
	s.Actions = &fakeActions{runs: map[int64]*fakeRun{}}
	jit.jobRunning = true
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	c.add(FailedStartAfter + time.Second)
	mustScale(t, s, nil)
	mustScale(t, s, startedRun(1, jit.calls[0], 9))
	if got := tagOf(f.tasks[0], "paddock:job"); got != jit.calls[0]+"@9" {
		t.Fatalf("job tag %q", got)
	}
}

// spotKilled is a task the pool ran, whose runner was running run 5's job
// when Spot reclaimed it at `at`. The task stopped two minutes on, at the
// end of the warning.
func spotKilled(f *fakeAWS, runner string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks = append(f.tasks, ecstypes.Task{
		TaskArn:       aws.String(fmt.Sprintf("arn:aws:ecs:us-east-1:123456789012:task/paddock/%032d", len(f.tasks)+1)),
		StartedBy:     aws.String("paddock-5"),
		DesiredStatus: aws.String("STOPPED"),
		LastStatus:    aws.String("STOPPED"),
		StopCode:      ecstypes.TaskStopCodeSpotInterruption,
		StoppingAt:    aws.Time(at),
		StoppedAt:     aws.Time(at.Add(2 * time.Minute)),
		CreatedAt:     aws.Time(at.Add(-time.Hour)),
		Tags: []ecstypes.Tag{
			{Key: aws.String("paddock:runner"), Value: aws.String("paddock-first")},
			{Key: aws.String("paddock:handed"), Value: aws.String(fmt.Sprintf("%s@%d", runner, at.Add(-30*time.Minute).Unix()))},
			{Key: aws.String("paddock:job"), Value: aws.String(runner + "@5")},
		},
	})
}

func TestAJobSpotKilledIsReRunOnceItsRunCompletes(t *testing.T) {
	f, s, _, c, l := recovering(t)
	actions := &fakeActions{runs: map[int64]*fakeRun{5: {status: "in_progress", attempts: [][]github.Job{{
		{ID: 71, RunnerName: "paddock-other", Conclusion: "success"},
	}}}}}
	s.Actions = actions
	at := c.now().Add(-time.Minute)
	spotKilled(f, "paddock-killed", at)
	actions.runs[5].attempts[0] = append(actions.runs[5].attempts[0],
		github.Job{ID: 72, RunnerName: "paddock-killed", Conclusion: "failure", CompletedAt: at.Add(3 * time.Minute)})

	mustScale(t, s, assigned(0))
	if len(actions.reruns) != 0 {
		t.Fatal("re-ran a job before its run completed")
	}
	actions.runs[5].status = "completed"
	c.add(time.Minute)
	mustScale(t, s, nil)
	if len(actions.reruns) != 1 || actions.reruns[0] != 72 {
		t.Fatalf("re-ran %v", actions.reruns)
	}
	if len(l.lines("job killed by a Spot reclaim")) != 1 || len(l.lines("job re-run")) != 1 {
		t.Fatalf("log %s", l.buf.String())
	}
	// The re-run completes too: nothing more is owed.
	actions.runs[5].status = "completed"
	c.add(time.Minute)
	mustScale(t, s, nil)
	c.add(time.Minute)
	mustScale(t, s, nil)
	if len(actions.reruns) != 1 {
		t.Fatalf("re-ran %v: a job twice", actions.reruns)
	}
}

func TestARestartedListenerReRunsAKilledJobOnceAndNeverTwice(t *testing.T) {
	f, s, _, c, _ := recovering(t)
	at := c.now().Add(-time.Minute)
	actions := &fakeActions{runs: map[int64]*fakeRun{5: {status: "completed", attempts: [][]github.Job{{
		{ID: 72, RunnerName: "paddock-killed", Conclusion: "cancelled", CompletedAt: at.Add(3 * time.Minute)},
	}}}}}
	spotKilled(f, "paddock-killed", at)

	// A listener that knew nothing of the reclaim reads it back from ECS.
	s.Actions = actions
	mustScale(t, s, assigned(0))
	if len(actions.reruns) != 1 {
		t.Fatalf("re-ran %v", actions.reruns)
	}
	actions.runs[5].status = "completed"

	// One that restarts after the re-run finds the job's runner gone from
	// the run's latest attempt.
	next, _ := newScaler(f, spotTest)
	next.Actions = actions
	next.Now = c.now
	mustScale(t, next, assigned(0))
	if len(actions.reruns) != 1 {
		t.Fatalf("re-ran %v: a job twice across a restart", actions.reruns)
	}
}

// A run deleted from GitHub has no job left to re-run.
func TestAReclaimedJobWhoseRunIsDeletedIsOwedNoMore(t *testing.T) {
	f, s, _, c, _ := recovering(t)
	actions := &fakeActions{runs: map[int64]*fakeRun{}, gone: map[int64]bool{5: true}}
	s.Actions = actions
	spotKilled(f, "paddock-killed", c.now().Add(-time.Minute))
	mustScale(t, s, assigned(0))
	c.add(time.Minute)
	mustScale(t, s, nil)
	if actions.reads != 1 || len(actions.reruns) != 0 {
		t.Fatalf("read the deleted run %d times, re-ran %v", actions.reads, actions.reruns)
	}
}

// A stopped task already read is not described again each minute.
func TestAStoppedTaskIsDescribedOnce(t *testing.T) {
	f, s, _, c, _ := recovering(t)
	s.Actions = &fakeActions{runs: map[int64]*fakeRun{5: {status: "in_progress", attempts: [][]github.Job{{}}}}}
	spotKilled(f, "paddock-killed", c.now().Add(-time.Minute))
	arn := aws.ToString(f.tasks[0].TaskArn)
	mustScale(t, s, assigned(0))
	c.add(time.Minute)
	mustScale(t, s, nil)
	n := 0
	for _, d := range f.described {
		if d == arn {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("described the stopped task %d times", n)
	}
}

func TestAJobThatFinishesInsideTheWarningIsCountedAndNotReRun(t *testing.T) {
	f, s, _, c, l := recovering(t)
	at := c.now().Add(-time.Minute)
	actions := &fakeActions{runs: map[int64]*fakeRun{5: {status: "completed", attempts: [][]github.Job{{
		{ID: 72, RunnerName: "paddock-killed", Conclusion: "success", CompletedAt: at.Add(time.Minute)},
	}}}}}
	s.Actions = actions
	spotKilled(f, "paddock-killed", at)
	mustScale(t, s, assigned(0))
	if len(actions.reruns) != 0 {
		t.Fatalf("re-ran %v", actions.reruns)
	}
	if len(l.lines("job finished inside the warning")) != 1 {
		t.Fatalf("log %s", l.buf.String())
	}
}

// A job that ends by itself inside the warning, red or cancelled, ends
// before its task stops: it was not killed, and is not re-run.
func TestAJobThatFailsByItselfInsideTheWarningIsNotReRun(t *testing.T) {
	for _, conclusion := range []string{"failure", "cancelled"} {
		f, s, _, c, l := recovering(t)
		at := c.now().Add(-time.Minute)
		actions := &fakeActions{runs: map[int64]*fakeRun{5: {status: "completed", attempts: [][]github.Job{{
			{ID: 72, RunnerName: "paddock-killed", Conclusion: conclusion, CompletedAt: at.Add(time.Minute)},
		}}}}}
		s.Actions = actions
		spotKilled(f, "paddock-killed", at)
		mustScale(t, s, assigned(0))
		if len(actions.reruns) != 0 || len(l.lines("job killed by a Spot reclaim")) != 0 {
			t.Errorf("%s: re-ran %v; log %s", conclusion, actions.reruns, l.buf.String())
		}
		if len(l.lines("job finished inside the warning")) != 1 {
			t.Errorf("%s: not counted as finished inside the warning", conclusion)
		}
	}
}

func TestATaskThatWasWaitingWhenReclaimedOwesNothing(t *testing.T) {
	f, s, _, c, l := recovering(t)
	at := c.now().Add(-time.Minute)
	actions := &fakeActions{runs: map[int64]*fakeRun{5: {status: "completed", attempts: [][]github.Job{{
		{ID: 72, RunnerName: "paddock-killed", Conclusion: "failure", CompletedAt: at.Add(-time.Minute)},
	}}}}}
	s.Actions = actions
	spotKilled(f, "paddock-killed", at)
	mustScale(t, s, assigned(0))
	if len(actions.reruns) != 0 || len(l.lines("job finished inside the warning")) != 0 {
		t.Fatalf("re-ran %v; log %s", actions.reruns, l.buf.String())
	}
}

func TestEachFinishedJobIsCountedForItsPool(t *testing.T) {
	_, s, _, _, l := recovering(t)
	mustScale(t, s, finished(0, "paddock-a", "paddock-b"))
	got := l.lines("job finished")
	if len(got) != 2 || got[0]["repo"] != "acme/widgets" || got[0]["pool"] != "test" {
		t.Fatalf("logged %v", got)
	}
}

func TestAJobWaitingFiveMinutesForARunnerIsLogged(t *testing.T) {
	_, s, _, c, l := recovering(t)
	msg := assigned(1)
	msg.Statistics.TotalAssignedJobs = 1
	msg.JobAssignedMessages = []*scaleset.JobAssigned{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 9}}}
	mustScale(t, s, msg)
	c.add(JobWaitLimit - time.Second)
	mustScale(t, s, nil)
	if len(l.lines("job waiting for a runner")) != 0 {
		t.Fatal("logged a job waiting under five minutes")
	}
	c.add(2 * time.Second)
	mustScale(t, s, nil)
	if got := l.lines("job waiting for a runner"); len(got) != 1 || got[0]["jobs"] != 1.0 {
		t.Fatalf("logged %v", got)
	}
	mustScale(t, s, &scaleset.RunnerScaleSetMessage{
		Statistics:         &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1, TotalRunningJobs: 1},
		JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 9}}},
	})
	c.add(time.Minute)
	mustScale(t, s, nil)
	if got := l.lines("job waiting for a runner"); len(got) != 1 {
		t.Fatalf("logged a started job as waiting: %v", got)
	}
}

// GitHub sends no message again for a job already assigned, so a listener
// that restarts times the jobs GitHub's counts show waiting from its start.
func TestAJobWaitingAcrossARestartIsStillLogged(t *testing.T) {
	_, s, _, c, l := recovering(t)
	mustScale(t, s, &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3, TotalRunningJobs: 2}})
	c.add(JobWaitLimit + time.Second)
	mustScale(t, s, nil)
	if got := l.lines("job waiting for a runner"); len(got) != 1 || got[0]["jobs"] != 1.0 {
		t.Fatalf("logged %v", got)
	}
	// Once GitHub's counts show none waiting, nothing is logged.
	mustScale(t, s, &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3, TotalRunningJobs: 3}})
	c.add(time.Minute)
	mustScale(t, s, nil)
	if got := l.lines("job waiting for a runner"); len(got) != 1 {
		t.Fatalf("logged %v after the job started", got)
	}
}

func TestATaskRunningPastItsPoolsMaximumJobTimeIsLogged(t *testing.T) {
	f, s, jit, c, l := recovering(t)
	s.Pool.MaxJob = time.Hour
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	mustScale(t, s, startedRun(1, jit.calls[0], 5))
	c.add(time.Hour + WarmLimit - time.Second)
	mustScale(t, s, nil)
	if len(l.lines("task past its pool's maximum job time")) != 0 {
		t.Fatal("logged a task inside its time")
	}
	c.add(2 * time.Second)
	mustScale(t, s, nil)
	if len(l.lines("task past its pool's maximum job time")) != 1 {
		t.Fatalf("log %s", l.buf.String())
	}
	if len(f.stopped) != 0 {
		t.Fatal("stopped a task past its time: the listener stops only a failed start")
	}
}

func TestAPoolsSpotKillsOverOneIn200Over30DaysAreLogged(t *testing.T) {
	for _, tc := range []struct {
		finished, killed float64
		over             bool
	}{{1000, 5, false}, {1000, 6, true}, {0, 0, false}} {
		_, s, _, c, l := recovering(t)
		m := &fakeMetrics{sums: map[string]float64{"JobsFinished": tc.finished, "JobsKilledBySpot": tc.killed}}
		s.Metrics = m
		mustScale(t, s, assigned(0))
		if got := len(l.lines("spot kills over their limit")); (got == 1) != tc.over {
			t.Errorf("%v of %v killed: logged %d times", tc.killed, tc.finished, got)
		}
		q := m.asked[0].MetricDataQueries[0].MetricStat
		if aws.ToInt32(q.Period) != 30*24*3600 || aws.ToString(q.Metric.Namespace) != MetricNamespace {
			t.Errorf("asked %+v", q)
		}
		dims := map[string]string{}
		for _, d := range q.Metric.Dimensions {
			dims[aws.ToString(d.Name)] = aws.ToString(d.Value)
		}
		if dims["Repo"] != "acme/widgets" || dims["Pool"] != "test" {
			t.Errorf("dimensions %v", dims)
		}
		// Checked once an hour, not each poll.
		c.add(time.Minute)
		mustScale(t, s, nil)
		if len(m.asked) != 1 {
			t.Errorf("asked %d times in a minute", len(m.asked))
		}
	}
}

func TestARunnerTaskGetsSpotsFullWarningToStop(t *testing.T) {
	f, s, _, _, _ := recovering(t)
	mustScale(t, s, assigned(1))
	def := f.defs[aws.ToString(f.runs[0].TaskDefinition)]
	if got := aws.ToInt32(def.ContainerDefinitions[0].StopTimeout); got != 120 {
		t.Fatalf("stop timeout %d", got)
	}
}

// ECS lists a task as stopped from the start of its warning, before it has
// stopped. It is read again once it has, so its job's end can be weighed
// against its stop.
func TestATaskStillInItsWarningIsReadAgainOnceItStops(t *testing.T) {
	f, s, _, c, _ := recovering(t)
	at := c.now().Add(-time.Minute)
	actions := &fakeActions{runs: map[int64]*fakeRun{5: {status: "completed", attempts: [][]github.Job{{
		{ID: 72, RunnerName: "paddock-killed", Conclusion: "failure", CompletedAt: at.Add(3 * time.Minute)},
	}}}}}
	s.Actions = actions
	spotKilled(f, "paddock-killed", at)
	f.mu.Lock()
	stopped := f.tasks[len(f.tasks)-1].StoppedAt
	f.tasks[len(f.tasks)-1].StoppedAt = nil
	f.mu.Unlock()
	mustScale(t, s, assigned(0))
	if len(actions.reruns) != 0 {
		t.Fatal("weighed a job against a task that had not stopped")
	}
	f.mu.Lock()
	f.tasks[len(f.tasks)-1].StoppedAt = stopped
	f.mu.Unlock()
	c.add(time.Minute)
	mustScale(t, s, nil)
	if len(actions.reruns) != 1 {
		t.Fatalf("re-ran %v once the task stopped", actions.reruns)
	}
}

// A task keeps the maximum job time it started under, through a tag a
// restarted listener reads back: a pool's later change applies to its next
// tasks only.
func TestATaskKeepsTheMaximumJobTimeItStartedUnder(t *testing.T) {
	f, s, jit, c, l := recovering(t)
	s.Pool.MaxJob = 2 * time.Hour
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	mustScale(t, s, startedRun(1, jit.calls[0], 5))
	s.Pool.MaxJob = 30 * time.Minute
	c.add(time.Hour + WarmLimit)
	mustScale(t, s, nil)
	// And as a restarted listener reads it.
	s.tasks = nil
	mustScale(t, s, nil)
	if got := l.lines("task past its pool's maximum job time"); len(got) != 0 {
		t.Fatalf("logged a task inside the time it started under: %v", got)
	}
	c.add(time.Hour + time.Second)
	mustScale(t, s, nil)
	if got := l.lines("task past its pool's maximum job time"); len(got) != 1 || got[0]["maxJob"] != "2h0m0s" {
		t.Fatalf("log %s", l.buf.String())
	}
}
