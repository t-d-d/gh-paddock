# A pool per repo
Relevance: Scale sets, runner groups and runner tasks, and how a task wipes between jobs: internal/repos, internal/serve, internal/github, internal/runners, runner/
Decision: Every pool belongs to one repo, and a runner task only runs jobs from its own pool.

Why: Client repos share a deployment. A runner that took jobs from two repos
could leak one repo's code or secrets into another's job, and a pool shared
by two repos would put one client's cost on another's tag.

## Rules

- Each pool has its own scale set and its own runner group, both named
  `paddock-<repo>-<pool>`. The runner group is open to that one repo, with
  public repos off. The listener narrows a group back if someone widens it.
- A runner's just-in-time config is made for its pool's scale set, so its
  runner takes jobs from that pool alone, and it runs one job.
- A task may run several runners in turn, each for one job, and every one
  of them from its own pool. A finished task waits up to 30 seconds, with no
  runner registered, for a config for a job already assigned to its pool,
  and exits by itself if none comes (remove-then-terminate.md).
- A waiting task counts toward its pool's maximum. The listener writes a
  config to a waiting task before it starts a new task, and starts new
  tasks only for the jobs beyond its waiting tasks.
- Before each runner, a task wipes the job's checkout and temporary
  directories. It keeps the tool cache and the home caches, such as npm's
  and Go's modules.
- The wipe does not separate two jobs in one task: a job can use sudo in the
  image, leave a process running, or change a kept cache. The pool is the
  boundary, so a pool takes only runs that hold the same credentials.
- The credentials that define a pool are the repo's secrets and the
  environments a run uses: the test pool takes runs that use no
  environment, and the dev pool takes runs in the dev environment. The
  per-run `GITHUB_TOKEN` does not count.
- A run that GitHub gives fewer secrets than the pool's other runs, such as
  Dependabot's or a fork pull request's, stays on GitHub's runners.
- The listener counts a pool's tasks by the scale set they were started for,
  so no pool's tasks count toward another's maximum.
- Each runner task is tagged with its repo and pool.
- A public repo is never served.

## Examples

- The Go subtests named `fitness: … (pool-per-repo.md)` in internal/repos,
  internal/github and internal/runners.
- `fitness: a task wipes the last job's checkout and temporary files and keeps the caches (pool-per-repo.md)`
  in test/runner-entrypoint.test.ts.

## Do not

- Do not share a runner group between a repo's pools, or between repos, to
  save on groups: the group is what keeps another repo's jobs off a runner.
- Do not reuse a runner for a second job: start a new runner from a new
  config for each one.
- Do not skip the wipe because the next job comes from the same pool: a job
  must not find the last job's checkout and take it for its own.
- Do not let a pool take runs that hold different credentials, such as
  Dependabot's or a fork pull request's beside the repo's own: a run
  without the secrets could leave something behind that reads the next
  job's.
- Do not keep an idle runner registered on a waiting task: it would need
  removing before the task stops, and the task could take a job as it is
  removed.

Judgment: no check sees GitHub's side: that a runner group really is closed to other repos. The listener sets it on every start. No check sees which runs a served repo's workflows send to paddock's labels, so no check sees Dependabot's or fork pull requests' runs kept off them.
History: Settled with the owner in PRD #1, and written by #16. #53 let a task run several runners in turn, as PRD #1's journey 2 says. #54 had a finished task wait with no runner registered and exit by itself, and named the credentials that define a pool.
