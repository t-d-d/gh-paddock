package runners

import (
	"context"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/aws/aws-sdk-go-v2/aws"
)

// finished is a message that a pool's runners finished their jobs, with
// the jobs GitHub now has assigned to the pool.
func finished(assignedJobs int, runners ...string) *scaleset.RunnerScaleSetMessage {
	msg := assigned(assignedJobs)
	for _, r := range runners {
		msg.JobCompletedMessages = append(msg.JobCompletedMessages, &scaleset.JobCompleted{RunnerName: r, Result: "succeeded"})
	}
	return msg
}

func started(assignedJobs int, runners ...string) *scaleset.RunnerScaleSetMessage {
	msg := assigned(assignedJobs)
	for _, r := range runners {
		msg.JobStartedMessages = append(msg.JobStartedMessages, &scaleset.JobStarted{RunnerName: r})
	}
	return msg
}

// clock is a scaler's clock the test moves.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func withClock(s *Scaler) *clock     { c := &clock{t: time.Now()}; s.Now = c.now; return c }
func objectKey(runner string) string { return "configs/5/" + runner }
func mustScale(t *testing.T, s *Scaler, msg *scaleset.RunnerScaleSetMessage) {
	t.Helper()
	if err := s.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
}

func TestAJobAssignedAsATaskFinishesRunsOnThatTask(t *testing.T) {
	f := newFakeAWS()
	s, jit := newScaler(f, spotTest)
	withClock(s)
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	first := jit.calls[0]

	mustScale(t, s, finished(1, first))
	if len(f.runs) != 1 {
		t.Fatalf("started %d new tasks for a job a waiting task could take", len(f.runs)-1)
	}
	later, ok := f.object(t, objectKey(first))
	if !ok {
		t.Fatal("no config written to the waiting task")
	}
	if len(jit.calls) != 2 || later.Runner != jit.calls[1] || later.JITConfig != "jit-for-"+jit.calls[1] {
		t.Fatalf("the waiting task was handed %+v", later)
	}
	if later.Runner == first {
		t.Fatal("the waiting task was handed its last runner again")
	}
	// The task now runs a job: a poll on the same counts hands it nothing
	// more and starts nothing.
	mustScale(t, s, nil)
	if len(f.runs) != 1 || len(jit.calls) != 2 {
		t.Fatalf("runs %d, configs %d after a quiet poll", len(f.runs), len(jit.calls))
	}
}

func TestConfigsGoToWaitingTasksFirstAndNewTasksOnlyForTheJobsBeyondThem(t *testing.T) {
	f := newFakeAWS()
	s, jit := newScaler(f, spotTest)
	withClock(s)
	mustScale(t, s, assigned(3))
	f.setLastStatus("RUNNING")
	firsts := append([]string(nil), jit.calls...)

	mustScale(t, s, finished(5, firsts...))
	handed := 0
	for _, r := range firsts {
		if _, ok := f.object(t, objectKey(r)); ok {
			handed++
		}
	}
	if handed != 3 {
		t.Fatalf("handed configs to %d of 3 waiting tasks", handed)
	}
	if len(f.runs) != 5 {
		t.Fatalf("started %d new tasks for the 2 jobs beyond 3 waiting tasks", len(f.runs)-3)
	}
}

func TestAWaitingTaskIsHandedAConfigOnlyForAJobAlreadyAssigned(t *testing.T) {
	f := newFakeAWS()
	s, jit := newScaler(f, spotTest)
	c := withClock(s)
	mustScale(t, s, assigned(2))
	f.setLastStatus("RUNNING")

	// One runner finishes; the other's job still runs, and no job waits.
	mustScale(t, s, finished(1, jit.calls[0]))
	if len(f.objPuts) != 0 || len(f.runs) != 2 {
		t.Fatalf("objects %v, runs %d with no job waiting", f.objPuts, len(f.runs))
	}

	// A job comes after the task's wait is over: the task has gone, or is
	// going, so the job gets a new task.
	c.add(31 * time.Second)
	mustScale(t, s, assigned(2))
	if len(f.objPuts) != 0 {
		t.Fatalf("handed a config to a task past its wait: %v", f.objPuts)
	}
	if len(f.runs) != 3 {
		t.Fatalf("started %d tasks for the new job", len(f.runs)-2)
	}
}

// A waiting task counts toward its pool's maximum until it exits.
func TestAWaitingTaskCountsTowardItsPoolsMaximum(t *testing.T) {
	f := newFakeAWS()
	pool := spotTest
	pool.MaxRunners = 2
	s, jit := newScaler(f, pool)
	c := withClock(s)
	mustScale(t, s, assigned(2))
	f.setLastStatus("RUNNING")
	mustScale(t, s, finished(1, jit.calls[0]))
	c.add(31 * time.Second)
	mustScale(t, s, assigned(3))
	if len(f.runs) != 2 {
		t.Fatalf("ran %d tasks in a pool of 2 while a task still exits", len(f.runs))
	}
}

func TestNoConfigIsHandedToATaskPastTheWarmLimit(t *testing.T) {
	f := newFakeAWS()
	s, jit := newScaler(f, spotTest)
	c := withClock(s)
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	c.add(WarmLimit + time.Second)

	mustScale(t, s, finished(1, jit.calls[0]))
	if len(f.objPuts) != 0 {
		t.Fatalf("handed a config %s after the task started", WarmLimit)
	}
	if len(f.runs) != 2 {
		t.Fatal("the job got no new task")
	}
}

// GitHub keeps a record of a runner as soon as its config is made, so the
// record does not show it registered. A just-in-time runner takes its job
// as it registers: the job's start is the sign.
func TestALaterConfigsObjectIsDeletedOnceItsRunnerRegistersAndTakesItsJob(t *testing.T) {
	f := newFakeAWS()
	s, jit := newScaler(f, spotTest)
	withClock(s)
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	first := jit.calls[0]
	mustScale(t, s, finished(1, first))

	mustScale(t, s, nil)
	if _, ok := f.object(t, objectKey(first)); !ok {
		t.Fatal("the config was deleted before its runner registered")
	}
	mustScale(t, s, started(1, jit.calls[1]))
	if _, ok := f.object(t, objectKey(first)); ok {
		t.Fatal("the config is kept after its runner took a job")
	}
}

// A task can run several runners in turn: the second finishes, and the
// task takes a third.
func TestATaskTakesAConfigAfterEachJob(t *testing.T) {
	f := newFakeAWS()
	s, jit := newScaler(f, spotTest)
	withClock(s)
	mustScale(t, s, assigned(1))
	f.setLastStatus("RUNNING")
	first := jit.calls[0]
	mustScale(t, s, finished(1, first))
	mustScale(t, s, started(1, jit.calls[1]))
	mustScale(t, s, finished(1, jit.calls[1]))
	third, ok := f.object(t, objectKey(first))
	if !ok || len(jit.calls) != 3 || third.Runner != jit.calls[2] {
		t.Fatalf("the task's third config %+v", third)
	}
	if len(f.runs) != 1 {
		t.Fatalf("started %d tasks", len(f.runs))
	}
}

func TestAConfigHandedToATaskThatExitsUnstartedGetsANewTask(t *testing.T) {
	t.Run("fitness: a config handed to a task that exits unstarted gets a new task (remove-then-terminate.md)", func(t *testing.T) {
		f := newFakeAWS()
		s, jit := newScaler(f, spotTest)
		withClock(s)
		mustScale(t, s, assigned(1))
		f.setLastStatus("RUNNING")
		first := jit.calls[0]
		mustScale(t, s, finished(1, first))

		// The task exits before it reads the config; its runner never
		// registers, and the job is still assigned.
		f.stopAll()
		mustScale(t, s, nil)
		if len(f.runs) != 2 {
			t.Fatalf("the job got %d new tasks", len(f.runs)-1)
		}
		if _, ok := f.object(t, objectKey(first)); ok {
			t.Fatal("the gone task's config is kept")
		}
	})
}

// A waiting task's later configs go to its own object, which only its URL
// reaches: no parameter, no task definition and no other task's object.
func TestAWaitingTasksLaterConfigsComeOnlyThroughItsOwnURL(t *testing.T) {
	t.Run("fitness: a waiting task's later configs come only through its own URL (runner-isolation.md)", func(t *testing.T) {
		f := newFakeAWS()
		s, jit := newScaler(f, spotTest)
		withClock(s)
		mustScale(t, s, assigned(2))
		f.setLastStatus("RUNNING")
		params, defs, signed := len(f.paramPuts), len(f.defs), len(f.presigned)

		mustScale(t, s, finished(2, jit.calls[1]))
		if len(f.paramPuts) != params || len(f.defs) != defs || len(f.presigned) != signed {
			t.Fatalf("a later config wrote a parameter, a task definition or a new URL")
		}
		if len(f.objPuts) != 1 || f.objPuts[0] != "configs/"+f.presigned[1].key {
			t.Fatalf("wrote %v; the waiting task's URL reaches %s", f.objPuts, f.presigned[1].key)
		}
	})
}

func TestNoTaskTakesAConfigForAnotherPool(t *testing.T) {
	t.Run("fitness: a waiting task takes configs from its own pool alone (pool-per-repo.md)", func(t *testing.T) {
		f := newFakeAWS()
		test, testJIT := newScaler(f, spotTest)
		withClock(test)
		dev, devJIT := newScaler(f, spotTest)
		dev.Pool.Name = "dev"
		dev.ScaleSetID = 6
		withClock(dev)
		mustScale(t, test, assigned(1))
		f.setLastStatus("RUNNING")

		// The dev pool hears of the test runner's finish, as if a message
		// strayed, and has a job of its own.
		mustScale(t, dev, finished(1, testJIT.calls[0]))
		if len(f.objPuts) != 0 {
			t.Fatalf("the dev pool handed a config to the test pool's task: %v", f.objPuts)
		}
		if len(devJIT.calls) != 1 || len(f.runs) != 2 || aws.ToString(f.runs[1].StartedBy) != "paddock-6" {
			t.Fatal("the dev job got no task of its own pool")
		}
	})
}

// A restarted listener knows of no waiting task: it treats every running
// task as busy, and starts tasks as it did before warm reuse.
func TestARestartedListenerHandsNoConfigToATaskItDidNotSeeFinish(t *testing.T) {
	f := newFakeAWS()
	first, jit := newScaler(f, spotTest)
	withClock(first)
	mustScale(t, first, assigned(1))
	f.setLastStatus("RUNNING")

	second, secondJIT := newScaler(f, spotTest)
	withClock(second)
	mustScale(t, second, assigned(2))
	if len(f.objPuts) != 0 || len(f.runs) != 2 || len(secondJIT.calls) != 1 {
		t.Fatalf("objects %v, runs %d", f.objPuts, len(f.runs))
	}
	// It sees the first task's runner finish, and hands that task the next.
	mustScale(t, second, finished(2, jit.calls[0]))
	if _, ok := f.object(t, objectKey(jit.calls[0])); !ok {
		t.Fatal("the restarted listener did not hand the finished task a config")
	}
}
