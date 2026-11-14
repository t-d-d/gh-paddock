# GitHub is the queue
Relevance: How jobs reach the runners: the listener (cmd/listener, internal/serve, internal/runners), the stacks in infra/**, anything that would receive an event
Decision: Jobs wait in GitHub until a pool has room; the listener polls the scale-set API, with no webhook and no queue of our own.

Why: GitHub already holds every queued job, and its scale-set API says how
many a pool needs. A webhook receiver or a queue of our own would be a second
copy of that state to keep in step, a public endpoint to defend, and more to
pay for, for no job that GitHub's own queue does not already hold.

## Rules

- The listener reads a pool's demand from its message session's statistics,
  `TotalAssignedJobs`, and from nothing else. It does not count job messages.
- Each poll tells GitHub the pool's maximum, so GitHub assigns no more jobs
  than the pool can run. Extra jobs wait in GitHub.
- A listener that starts takes the assigned counts from GitHub and the
  running tasks from ECS. It keeps no state of its own that a restart would
  lose.
- No stack defines a queue, a stream, an event bus, a function, a load
  balancer or an API, and no security group lets anything in.
- paddock's GitHub App has no webhook.

## Examples

- `fitness: GitHub is the queue, with no webhook and no queue of our own (github-queue.md)`
  in test/stack.test.ts.
- internal/runners scales from the statistics alone, and its tests restart
  a listener against tasks already running.

## Do not

- Do not add a webhook receiver to start runners sooner: it is a public
  endpoint, and GitHub's long poll already answers within seconds.
- Do not keep a queue or a job ledger in AWS: it would drift from GitHub's,
  and a restart would have to reconcile the two.

Judgment: no check sees whether the App has a webhook set, since that lives in GitHub. The operator leaves it off.
History: Settled with the owner in PRD #1, and written by #16.
