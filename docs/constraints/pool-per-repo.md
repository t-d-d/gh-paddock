# A pool per repo
Relevance: Scale sets, runner groups and runner tasks: internal/repos, internal/serve, internal/github, internal/runners
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
- The listener counts a pool's tasks by the scale set they were started for,
  so no pool's tasks count toward another's maximum.
- Each runner task is tagged with its repo and pool.
- A public repo is never served.

## Examples

- The Go subtests named `fitness: … (pool-per-repo.md)` in internal/repos,
  internal/github and internal/runners.

## Do not

- Do not share a runner group between a repo's pools, or between repos, to
  save on groups: the group is what keeps another repo's jobs off a runner.
- Do not reuse a runner for a second job: its workspace keeps the first job's
  files.

Judgment: no check sees GitHub's side: that a runner group really is closed to other repos. The listener sets it on every start.
History: Settled with the owner in PRD #1, and written by #16.
