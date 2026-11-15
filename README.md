# gh-paddock

Self-hosted GitHub Actions runners for private repos, on AWS Fargate, driven by GitHub's runner scale-set API.

[![Deploy](https://github.com/t-d-d/gh-paddock/actions/workflows/deploy.yml/badge.svg?branch=main)](https://github.com/t-d-d/gh-paddock/actions/workflows/deploy.yml)
[![Licence: Apache-2.0](https://img.shields.io/badge/licence-Apache--2.0-blue)](LICENSE)

GitHub-hosted runners bill private repos per minute, rounded up per job, and start every job on a fresh machine. gh-paddock runs those jobs on Fargate tasks in an AWS account of their own instead, billed per second.

One listener task holds a message session on GitHub's scale-set API for each scale set it serves. Each response says how many jobs are assigned to that set. The listener starts a runner task for each job its runners cannot take, and hands the task a just-in-time config through Parameter Store. Every runner takes one job and deregisters. A task in a test or dev pool then starts another runner and waits 30 seconds for the next job, so a chain of stages reuses one task. The listener removes an idle runner through the API, which refuses while the runner has a job, and stops its task only after the removal succeeds.

Jobs are split into pools by the credentials they hold, and a task only runs jobs from its own pool. Public repos keep GitHub's runners: their standard minutes are free, and a fork's pull request must not reach these.

The [runner lane page](https://t-d-d.github.io/gh-paddock/runner-lane.html) shows the v1 design step by step: a push, warm reuse, remove-then-terminate, a listener interrupted, a failed start and a Spot reclaim.

The name comes from the paddock where racehorses are kept warm before a race.

## Status

Under construction. The [v1 PRD](https://github.com/t-d-d/gh-paddock/issues/1) sets the scope: test and dev pools on Fargate Spot. Today the listener serves each listed repo's pools: it makes a scale set and a runner group per pool, polls GitHub for the pool's jobs, and finds a runner for each job, up to the pool's maximum. A task runs one job per runner. When a job finishes, its task waits up to 30 seconds and takes the pool's next assigned job if one comes. Otherwise it exits, and a later job gets a new task. A deploy probes the listener and rolls both images back when the probe fails.

Jobs that need a Docker daemon or KVM, macOS jobs and public repos stay on GitHub's runners.

## Layout

| Path | What |
|---|---|
| `cmd/listener/` | The listener, in Go, and its Dockerfile |
| `internal/` | The listener's packages |
| `infra/bootstrap.cfn.yml` | The deploy role GitHub's OIDC assumes |
| `infra/paddock.cfn.yml` | The listener's service, what it needs to start runner tasks, its alarms and the alerts topic |
| `runner/` | The runner's image, built FROM GitHub's runner image |
| `scripts/` | The deploy's probe and stack guard, and the public-repo scan |
| `test/` | Tests of the stacks, the workflows and the scripts |
| `docs/constraints/` | The rules in force |
| `docs/pages/` | The runner lane page, published on GitHub Pages |

`npm run gate` runs every check offline: the public-repo scan, lint, types and the Node tests, then `gofmt`, `go vet` and the Go tests.

## Deploying your own

The deploy needs an AWS account that holds gh-paddock alone, in us-east-1, with a default VPC and no OIDC provider for GitHub yet. Once, as an administrator:

1. Apply `infra/bootstrap.cfn.yml` as the stack `paddock-bootstrap`, with `GitHubOwner`, `GitHubOwnerId`, `GitHubRepo` and `GitHubRepoId` set to your fork's names and numeric ids.
2. Put its `DeployRoleArn` output in the repository secret `AWS_DEPLOY_ROLE_ARN`.
3. Write the alert address to the Parameter Store parameter `/paddock/alert-email`, as a String, not a SecureString.
4. Write the GitHub App's key to the Parameter Store parameter `/paddock/github-app`, as a SecureString under the account's default key. Its value is one JSON object: `appId`, a JSON number, and `privateKey`, the PKCS#1 PEM GitHub gives you, with its line breaks as `\n` escapes. `jq -n --argjson id <app id> --rawfile key <key file> '{appId: $id, privateKey: $key}'` builds it. No stack creates this parameter, and only the listener reads it.
5. List the repos to serve in the Parameter Store parameter `/paddock/repos`, as a String: `{"repos": [{"repo": "owner/name", "pools": [{"name": "test", "capacity": "spot"}, {"name": "dev", "capacity": "on-demand"}]}]}`. A pool may also set `vcpu`, `memoryGb`, `maxRunners` and `maxJobMinutes`, which default to 2, 8, 10 and 120. A task still running 30 minutes past its pool's `maxJobMinutes` raises an alarm. The App must be installed on each repo's org, and each repo must be private. The listener reads the list every two minutes and serves a change within five, with no restart. A removed pool takes no new jobs, and its scale set is deleted once its last tasks finish. A list it cannot read leaves the last good one in force and raises an alarm. Each pool's workflows run on `runs-on: paddock-<repo>-<pool>`.

From then on every green merge to main deploys. The operator guide comes with v1.

## Cost per repo

Of the tags the listener puts on every runner task it starts, two say whose cost it is. `paddock:repo` holds the repo as `owner/name`, and `paddock:pool` holds the pool's name. Cost Explorer can split the tasks' Fargate cost by either key once both are cost allocation tags. To turn them on:

1. Sign in to the organisation's management account. If the CI account is in an AWS Organization, only that account can turn on cost allocation tags. A standalone CI account is its own management account.
2. Open Billing and Cost Management, then Cost allocation tags, and find `paddock:repo` and `paddock:pool` under the user-defined tags. A key shows there only after a task carrying it has run, which can take up to 24 hours.
3. Select both keys and choose Activate. AWS takes up to 24 hours to apply them.

To read cost per repo, open Cost Explorer, filter Service to EC2 Container Service, which is how Cost Explorer names ECS. Group by the tag `paddock:repo`. To split one repo's cost by pool, filter that tag to the repo and group by `paddock:pool`. The listener's own task, the listener's and the runners' logs, Parameter Store, the runner-config bucket and the tasks' public IPv4 addresses carry neither tag, so they show as cost of no repo.

## Licence

[Apache-2.0](LICENSE).
