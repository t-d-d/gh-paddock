# Remove, then terminate
Relevance: When a runner task ends, and who ends it: the runner image's entrypoint in runner/, internal/runners, the listener's role in infra/**
Decision: A task exits only with no runner registered, and the listener stops a task only once its runner is removed or never registered.

Why: A registered runner can be handed a job at any moment. A task stopped
under a registered runner can take a job and die with it, and the job then
fails or waits for GitHub to notice the runner is gone. Ending a task only
when nothing is registered on it means no job is ever lost to a stop.

## Rules

- A task's runner takes one job. When it finishes, it leaves GitHub, and
  only then does the entrypoint wait for a new config.
- A waiting task holds no runner registered. It exits by itself when no new
  config comes in 30 seconds, or its URL is refused (runner-image.md).
- The listener stops a task only once that task's runner is removed from
  GitHub, or never registered. It stops only a failed start: a task whose
  runner has not taken its job three minutes after its config was handed
  over, at the task's start or through its URL. It first removes the runner
  record that config made, then stops the task, and the job gets a new
  task. If GitHub will not remove the record, the runner may have just
  taken its job, and the task is left. Every other task ends itself.
- On SIGTERM a waiting task exits at once, and a runner with no job yet is
  told to stop and leaves GitHub. A task whose runner has a job holds the
  signal back from it, so a job near its end finishes inside
  Fargate Spot's two minutes. A job still running when Spot stops the task
  dies with it, and the listener re-runs that job once its run completes.
- A config written to a task that exits without starting its runner is made
  good: the job still counts as assigned, so the listener starts a new task
  for it.

## Examples

- `fitness: a task exits only with no runner registered (remove-then-terminate.md)`
  in test/runner-entrypoint.test.ts.
- `fitness: the listener stops only runner tasks whose runners never took their jobs (remove-then-terminate.md)`
  in test/stack.test.ts, and the Go subtests
  `fitness: the listener stops only a task whose runner never took its job, once its runner is removed (remove-then-terminate.md)`
  in internal/runners.
- `fitness: a config handed to a task that exits unstarted gets a new task (remove-then-terminate.md)`
  in internal/runners.

## Do not

- Do not keep an idle runner registered on a waiting task to take the next
  job sooner: it would need removing before the task stops, and could take
  a job as it is removed.
- Do not use the listener's `ecs:StopTask` to end idle or waiting tasks, or
  one running past its time: a task ends itself, and a stop under a
  registered runner can kill a job. A task past its pool's job time raises
  an alarm and is left running.
- Do not stop a failed start before its runner record is removed: the
  runner could take a job in between and die with it.

History: Settled with the owner in #54. #19 narrowed "in v1 it stops none" to failed starts, as PRD #1's journey 4 asks, and had a task hold SIGTERM back from a running job.
