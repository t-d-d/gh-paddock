package runners

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/t-d-d/gh-paddock/internal/github"
)

// FailedStartAfter is how long a runner has to take its job once its config
// is handed over. One that has not is a failed start: the listener removes
// the runner's record, then stops its task, and the job gets a new one
// (docs/constraints/remove-then-terminate.md).
const FailedStartAfter = 3 * time.Minute

// JobWaitLimit is how long a job may wait for a runner before it is logged,
// which raises an alarm.
const JobWaitLimit = 5 * time.Minute

// MetricNamespace holds the metrics the stack's log filters make of the
// listener's log.
const MetricNamespace = "paddock"

// The log messages the stack's metric filters count. A test in test/ holds
// the stack's filters to these.
const (
	logJobFinished    = "job finished"
	logFailedStart    = "failed start"
	logJobWaiting     = "job waiting for a runner"
	logTaskPastMaxJob = "task past its pool's maximum job time"
	logInsideWarning  = "job finished inside the warning"
	logSpotKilled     = "job killed by a Spot reclaim"
	logReRun          = "job re-run"
	logSpotKillRate   = "spot kills over their limit"
)

// spotKillLimit is the share of a pool's jobs over 30 days that Spot may
// kill before an alarm.
const spotKillLimit = 1.0 / 200

// The tags a task carries beside its repo, pool and first runner, so a
// listener that restarts reads back what the last one knew: the runner last
// handed to it, and when, and the runner that last took a job, and its run.
const (
	tagHanded = "paddock:handed"
	tagJob    = "paddock:job"
	// tagMaxJob is the pool's maximum job time, in minutes, when the task
	// started: a later change to the pool applies to its next tasks only.
	tagMaxJob = "paddock:maxJobMinutes"
)

const (
	checkReclaimsEvery = time.Minute
	checkRateEvery     = time.Hour
	logEvery           = time.Minute
	defaultMaxJob      = 2 * time.Hour
	spotKillWindow     = 30 * 24 * time.Hour
)

// Actions is the part of GitHub's Actions API that re-runs a killed job.
type Actions interface {
	Run(ctx context.Context, owner, name string, runID int64) (github.Run, error)
	RunJobs(ctx context.Context, owner, name string, runID int64, attempt int) ([]github.Job, error)
	RerunJob(ctx context.Context, owner, name string, jobID int64) error
}

// Metrics reads the pool's counts back from CloudWatch.
type Metrics interface {
	GetMetricData(context.Context, *cloudwatch.GetMetricDataInput, ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// recovery is what the scaler keeps to recover failures. None of it needs
// to survive a restart: ECS's stopped tasks and the tasks' tags give it
// back.
type recovery struct {
	failedInARow int
	// owed are the jobs a Spot reclaim may have killed, by runner, until
	// their runs complete.
	owed map[string]*owedRun
	// seenStopped are the stopped tasks already read, while ECS lists them.
	seenStopped     map[string]bool
	reclaimsChecked time.Time
	rateChecked     time.Time
	// waiting are the jobs assigned or offered and not yet started, by
	// runner request, from when they were queued.
	waiting    map[int64]time.Time
	waitLogged time.Time
	// unheardSince is when GitHub's counts first showed more jobs waiting
	// than messages named, as after a restart: GitHub sends no message
	// again for a job already assigned.
	unheardSince time.Time
}

type owedRun struct {
	runner     string
	runID      int64
	stoppingAt time.Time // when Spot's warning began
	stoppedAt  time.Time // when the task stopped, at the warning's end or sooner
	killed     bool      // logged as killed, so counted once
}

// readTask is what a live task's tags say of it.
func (s *Scaler) readTask(lt ecstypes.Task) *task {
	first := tagOf(lt, "paddock:runner")
	created := s.now()
	if lt.CreatedAt != nil {
		created = *lt.CreatedAt
	}
	t := &task{arn: aws.ToString(lt.TaskArn), key: s.objectKey(first), runner: first, created: created, handed: created}
	if runner, at, ok := splitTag(tagOf(lt, tagHanded)); ok {
		t.runner, t.handed = runner, time.Unix(at, 0)
	}
	if runner, _, ok := splitTag(tagOf(lt, tagJob)); ok && runner == t.runner {
		t.started = true
	}
	if mins, err := strconv.Atoi(tagOf(lt, tagMaxJob)); err == nil && mins > 0 {
		t.maxJob = time.Duration(mins) * time.Minute
	}
	return t
}

// splitTag reads a tag of the form runner@number.
func splitTag(v string) (string, int64, bool) {
	i := strings.LastIndex(v, "@")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(v[i+1:], 10, 64)
	return v[:i], n, err == nil
}

func (s *Scaler) tag(ctx context.Context, t *task, key, value string) {
	if _, err := s.ECS.TagResource(ctx, &ecs.TagResourceInput{
		ResourceArn: aws.String(t.arn),
		Tags:        []ecstypes.Tag{{Key: aws.String(key), Value: aws.String(value)}},
	}); err != nil {
		s.Log.Warn("could not tag a runner task", "runner", t.runner, "tag", key, "error", err.Error())
	}
}

func shortARN(arn string) string { return arn[strings.LastIndex(arn, "/")+1:] }

// replaceFailedStarts removes the runner record of each task whose runner
// has not taken its job FailedStartAfter its config was handed over, then
// stops the task. A runner GitHub will not remove may have just taken its
// job, so its task is left. It returns the live tasks left running; the
// scale that follows starts a task for each stopped one's job.
func (s *Scaler) replaceFailedStarts(ctx context.Context, live []ecstypes.Task) []ecstypes.Task {
	now := s.now()
	stopped := map[string]bool{}
	for arn, t := range s.tasks {
		if t.started || now.Sub(t.handed) <= FailedStartAfter {
			continue
		}
		ref, err := s.JIT.GetRunnerByName(ctx, t.runner)
		if err != nil {
			s.Log.Warn("could not find a runner that never took its job", "runner", t.runner, "error", err.Error())
			continue
		}
		if ref != nil {
			if err := s.JIT.RemoveRunner(ctx, int64(ref.ID)); err != nil {
				if errors.Is(err, scaleset.JobStillRunningError) {
					// The runner took its job, though no message said so.
					t.started = true
					s.Log.Info("a runner thought never to have taken its job is running one; its task is left", "runner", t.runner)
					continue
				}
				s.Log.Warn("could not remove a runner that never took its job; its task is left", "runner", t.runner, "error", err.Error())
				continue
			}
		}
		if _, err := s.ECS.StopTask(ctx, &ecs.StopTaskInput{
			Cluster: aws.String(s.Launch.Cluster),
			Task:    aws.String(arn),
			Reason:  aws.String("paddock: its runner never took its job"),
		}); err != nil {
			s.Log.Warn("could not stop a task whose runner never took its job", "runner", t.runner, "error", err.Error())
			continue
		}
		if t.pending {
			s.deleteObject(ctx, t)
		}
		delete(s.tasks, arn)
		stopped[arn] = true
		s.failedInARow++
		s.Log.Warn(logFailedStart, "runner", t.runner, "task", shortARN(arn), "inARow", s.failedInARow)
	}
	if len(stopped) == 0 {
		return live
	}
	var left []ecstypes.Task
	for _, lt := range live {
		if !stopped[aws.ToString(lt.TaskArn)] {
			left = append(left, lt)
		}
	}
	return left
}

// watch logs what the alarms count, and settles the re-runs owed. Nothing it
// finds stops a task.
func (s *Scaler) watch(ctx context.Context) {
	now := s.now()
	// A task is handed configs for WarmLimit, 30 minutes, after its start,
	// and its last job may then run the pool's maximum as it was when the
	// task started.
	for _, t := range s.tasks {
		maxJob := t.maxJob
		if maxJob == 0 {
			maxJob = s.Pool.MaxJob
		}
		if maxJob == 0 {
			maxJob = defaultMaxJob
		}
		if age := now.Sub(t.created); age > WarmLimit+maxJob && now.Sub(t.warned) >= logEvery {
			t.warned = now
			s.Log.Warn(logTaskPastMaxJob, "task", shortARN(t.arn), "runner", t.runner,
				"age", age.Round(time.Minute).String(), "maxJob", maxJob.String())
		}
	}
	s.watchWaits(now)
	s.checkReclaims(ctx, now)
	s.checkSpotRate(ctx, now)
}

// hearWaits notes the jobs a message says wait for a runner, and those that
// started or finished.
func (s *Scaler) hearWaits(msg *scaleset.RunnerScaleSetMessage) {
	if s.waiting == nil {
		s.waiting = map[int64]time.Time{}
	}
	wait := func(job scaleset.JobMessageBase) {
		if _, ok := s.waiting[job.RunnerRequestID]; ok {
			return
		}
		at := job.QueueTime
		if at.IsZero() || at.After(s.now()) {
			at = s.now()
		}
		s.waiting[job.RunnerRequestID] = at
	}
	for _, job := range msg.JobAvailableMessages {
		wait(job.JobMessageBase)
	}
	for _, job := range msg.JobAssignedMessages {
		wait(job.JobMessageBase)
	}
	for _, job := range msg.JobStartedMessages {
		delete(s.waiting, job.RunnerRequestID)
	}
	for _, job := range msg.JobCompletedMessages {
		delete(s.waiting, job.RunnerRequestID)
	}
	// GitHub's counts are the backstop for a job cancelled with no message.
	if st := msg.Statistics; st != nil && st.TotalAvailableJobs+st.TotalAcquiredJobs+st.TotalAssignedJobs-st.TotalRunningJobs <= 0 {
		clear(s.waiting)
	}
}

func (s *Scaler) watchWaits(now time.Time) {
	long := 0
	for _, at := range s.waiting {
		if now.Sub(at) >= JobWaitLimit {
			long++
		}
	}
	unheard := 0
	if s.last != nil {
		unheard = s.last.TotalAssignedJobs - s.last.TotalRunningJobs - len(s.waiting)
	}
	switch {
	case unheard <= 0:
		s.unheardSince = time.Time{}
	case s.unheardSince.IsZero():
		s.unheardSince = now
	case now.Sub(s.unheardSince) >= JobWaitLimit:
		long += unheard
	}
	if long > 0 && now.Sub(s.waitLogged) >= logEvery {
		s.waitLogged = now
		s.Log.Warn(logJobWaiting, "jobs", long)
	}
}

// checkReclaims reads the pool's stopped tasks from ECS, once a minute, and
// owes a re-run for the job each Spot-reclaimed task's runner was running.
// Each owed job is settled once its run completes. A listener that restarts
// reads the reclaims back from ECS, which lists a stopped task for about an
// hour, and from the run's attempts, so it owes nothing twice.
func (s *Scaler) checkReclaims(ctx context.Context, now time.Time) {
	if s.Actions == nil || (!s.reclaimsChecked.IsZero() && now.Sub(s.reclaimsChecked) < checkReclaimsEvery) {
		return
	}
	s.reclaimsChecked = now
	if s.owed == nil {
		s.owed, s.seenStopped = map[string]*owedRun{}, map[string]bool{}
	}
	if stopped, listed, err := s.newlyStopped(ctx); err != nil {
		s.Log.Warn("could not read the pool's stopped tasks", "error", err.Error())
	} else {
		for _, t := range stopped {
			// A task in its warning is listed as stopped before it stops:
			// it is read again once it has.
			if t.StoppedAt == nil {
				continue
			}
			s.seenStopped[aws.ToString(t.TaskArn)] = true
			runner, runID, ok := splitTag(tagOf(t, tagJob))
			if t.StopCode != ecstypes.TaskStopCodeSpotInterruption || !ok || t.StoppingAt == nil {
				continue
			}
			s.owed[runner] = &owedRun{runner: runner, runID: runID, stoppingAt: *t.StoppingAt, stoppedAt: *t.StoppedAt}
		}
		for arn := range s.seenStopped {
			if !listed[arn] {
				delete(s.seenStopped, arn)
			}
		}
	}
	for runner, o := range s.owed {
		if s.settle(ctx, o) {
			delete(s.owed, runner)
		}
	}
}

// newlyStopped lists the pool's stopped tasks, and describes only those not
// read before. It returns those it described and every ARN ECS listed.
func (s *Scaler) newlyStopped(ctx context.Context) ([]ecstypes.Task, map[string]bool, error) {
	arns, err := s.taskARNs(ctx, ecstypes.DesiredStatusStopped)
	if err != nil {
		return nil, nil, err
	}
	listed := map[string]bool{}
	var unseen []string
	for _, arn := range arns {
		listed[arn] = true
		if !s.seenStopped[arn] {
			unseen = append(unseen, arn)
		}
	}
	tasks, err := s.describeTasks(ctx, unseen)
	return tasks, listed, err
}

// settle re-runs a reclaimed task's job once its run completes, if the job
// was killed: it failed, and GitHub marked it ended after its task stopped. It is done once the run's latest attempt no longer holds the
// job's runner: someone, or this listener, has re-run it.
func (s *Scaler) settle(ctx context.Context, o *owedRun) bool {
	log := s.Log.With("runner", o.runner, "run", o.runID)
	run, err := s.Actions.Run(ctx, s.Repo.Owner, s.Repo.Name, o.runID)
	if status := (*github.StatusError)(nil); errors.As(err, &status) && status.Status == http.StatusNotFound {
		// The run was deleted: there is no job left to re-run.
		log.Info("a reclaimed job's run is gone")
		return true
	}
	if err != nil {
		log.Warn("could not read a reclaimed job's run", "error", err.Error())
		return false
	}
	if run.Status != "completed" {
		return false
	}
	jobs, err := s.Actions.RunJobs(ctx, s.Repo.Owner, s.Repo.Name, o.runID, run.Attempt)
	if err != nil {
		log.Warn("could not read a reclaimed job's run", "error", err.Error())
		return false
	}
	for _, j := range jobs {
		if j.RunnerName != o.runner {
			continue
		}
		// A job that ends by itself, green, red or cancelled, ends before
		// its task stops; a killed one is marked ended only after.
		switch {
		case j.CompletedAt.Before(o.stoppingAt):
			// The task was waiting, its job done, when Spot took it.
			return true
		case j.Conclusion == "success" || !j.CompletedAt.After(o.stoppedAt):
			log.Info(logInsideWarning, "job", j.ID, "conclusion", j.Conclusion)
			return true
		}
		if !o.killed {
			o.killed = true
			log.Warn(logSpotKilled, "job", j.ID, "conclusion", j.Conclusion)
		}
		if err := s.Actions.RerunJob(ctx, s.Repo.Owner, s.Repo.Name, j.ID); err != nil {
			log.Warn("could not re-run a job a Spot reclaim killed", "job", j.ID, "error", err.Error())
			return false
		}
		log.Info(logReRun, "job", j.ID, "attempt", run.Attempt+1)
		return true
	}
	return true
}

// checkSpotRate reads the pool's finished and Spot-killed jobs over 30 days,
// once an hour, and logs when Spot killed more than 1 in 200. An alarm
// cannot look back 30 days itself: CloudWatch caps its window at 7.
func (s *Scaler) checkSpotRate(ctx context.Context, now time.Time) {
	if s.Metrics == nil || (!s.rateChecked.IsZero() && now.Sub(s.rateChecked) < checkRateEvery) {
		return
	}
	s.rateChecked = now
	dims := []cwtypes.Dimension{
		{Name: aws.String("Repo"), Value: aws.String(s.Repo.FullName())},
		{Name: aws.String("Pool"), Value: aws.String(s.Pool.Name)},
	}
	query := func(id, metric string) cwtypes.MetricDataQuery {
		return cwtypes.MetricDataQuery{Id: aws.String(id), MetricStat: &cwtypes.MetricStat{
			Metric: &cwtypes.Metric{Namespace: aws.String(MetricNamespace), MetricName: aws.String(metric), Dimensions: dims},
			Period: aws.Int32(int32(spotKillWindow / time.Second)),
			Stat:   aws.String("Sum"),
		}}
	}
	out, err := s.Metrics.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		StartTime:         aws.Time(now.Add(-spotKillWindow)),
		EndTime:           aws.Time(now),
		MetricDataQueries: []cwtypes.MetricDataQuery{query("finished", "JobsFinished"), query("killed", "JobsKilledBySpot")},
	})
	if err != nil {
		s.Log.Warn("could not read the pool's job counts", "error", err.Error())
		return
	}
	sums := map[string]float64{}
	for _, r := range out.MetricDataResults {
		for _, v := range r.Values {
			sums[aws.ToString(r.Id)] += v
		}
	}
	if sums["finished"] > 0 && sums["killed"]/sums["finished"] > spotKillLimit {
		s.Log.Warn(logSpotKillRate, "killed", sums["killed"], "finished", sums["finished"], "days", 30)
	}
}
