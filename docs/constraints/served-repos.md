# Served repos
Relevance: The list of served repos and its format: internal/repos, cmd/listener, the stacks in infra/**, the README's setup
Decision: Which repos are served, and their pools, is a Parameter Store parameter in the CI account, never a file here or a stack value.

Why: The list names private repos, which is the owner's business and not a
public repo's (public-repo.md). Kept in the account, it changes without a
pull request or a deploy.

## Rules

- The list is the String parameter `/paddock/repos`, written by the
  operator. No stack creates it, and no stack takes a repo as a parameter.
- Its value is one JSON object: `{"repos": [{"repo": "owner/name", "pools":
  [{"name": "test", "capacity": "spot"}]}]}`. A pool may also set `arch`,
  `vcpu`, `memoryGb`, `maxRunners` and `maxJobMinutes`; they default to
  `x64`, 2, 8, 10 and 120. `arch` is `x64` or `arm64` (runner-image.md).
- The listener reads the list on start and every two minutes after, and
  serves what changed with no restart. A missing or empty list serves
  nothing. An entry that is not valid is logged and skipped, and the rest
  are served; a repo whose entry is skipped stays as the last good list had
  it, and the entry raises the same alarm as an unreadable list.
- A list the listener cannot read or parse leaves the last good one in
  force, or nothing at the start, and raises an alarm through
  `AlertsTopic`. A failed read adds, changes and deletes nothing.
- A pool gone from the list stops polling at once, so its waiting tasks get
  no further config and exit by themselves (remove-then-terminate.md). Its
  scale set is deleted once no task of the pool is left. The listener finds
  a removed pool's scale set by its runner group, so a restart loses
  nothing.
- A changed pool setting applies to the next task the pool starts. Tasks
  already running keep the settings they started with.
- Tests use fake repos, such as `acme/widgets`.

## Examples

- `fitness: served repos are configuration in the CI account, never a file here (served-repos.md)`
  in test/stack.test.ts, and the public-repo scan.
- The Go subtest `fitness: a failed read of the list adds, changes and deletes nothing (served-repos.md)`
  in cmd/listener, and the manager's tests in internal/serve.

## Do not

- Do not put the list in a stack parameter resolved at deploy: a change to it
  would then need a deploy.
- Do not take a failed read, or a skipped entry, for an empty list: every
  scale set it served would be deleted.
- Do not log the list whole: a log line pasted into a public issue would
  carry every repo.

Judgment: the listener's logs name the repo a line is about, so the operator can act on it. A log line pasted into a public issue needs its repo cut first.
History: Settled with the owner in PRD #1, extending public-repo.md, and written by #16. #19 added each pool's `maxJobMinutes`. #20 had the listener re-read the list while it runs, keep the last good one when a read fails, and drain a removed pool, as PRD #1's journeys 1 and 6 ask. #70 added each pool's `arch`.
