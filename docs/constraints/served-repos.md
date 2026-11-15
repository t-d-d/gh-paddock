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
  [{"name": "test", "capacity": "spot"}]}]}`. A pool may also set `vcpu`,
  `memoryGb`, `maxRunners` and `maxJobMinutes`; they default to 2, 8, 10
  and 120.
- The listener reads the list on start. A missing or empty list serves
  nothing. A list it cannot read or parse serves nothing and is logged. An
  entry that is not valid is logged and skipped, and the rest are served.
- Tests use fake repos, such as `acme/widgets`.

## Examples

- `fitness: served repos are configuration in the CI account, never a file here (served-repos.md)`
  in test/stack.test.ts, and the public-repo scan.

## Do not

- Do not put the list in a stack parameter resolved at deploy: a change to it
  would then need a deploy.
- Do not log the list whole: a log line pasted into a public issue would
  carry every repo.

Judgment: the listener's logs name the repo a line is about, so the operator can act on it. A log line pasted into a public issue needs its repo cut first.
History: Settled with the owner in PRD #1, extending public-repo.md, and written by #16. #19 added each pool's `maxJobMinutes`.
