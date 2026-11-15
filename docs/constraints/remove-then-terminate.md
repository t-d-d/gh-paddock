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
  GitHub, or never registered. In v1 it stops none: every task ends itself.
- A config written to a task that exits without starting its runner is made
  good: the job still counts as assigned, so the listener starts a new task
  for it.

## Examples

- `fitness: a task exits only with no runner registered (remove-then-terminate.md)`
  in test/runner-entrypoint.test.ts.
- `fitness: the listener stops no task (remove-then-terminate.md)` in
  test/stack.test.ts.
- `fitness: a config handed to a task that exits unstarted gets a new task (remove-then-terminate.md)`
  in internal/runners.

## Do not

- Do not keep an idle runner registered on a waiting task to take the next
  job sooner: it would need removing before the task stops, and could take
  a job as it is removed.
- Do not give the listener `ecs:StopTask` to end idle tasks: a task ends
  itself, so there is nothing for the listener to stop.

History: Settled with the owner in #54.
