# gh-paddock

Self-hosted GitHub Actions runners for private repos, on AWS Fargate, driven by GitHub's runner scale-set API.

[![Deploy](https://github.com/t-d-d/gh-paddock/actions/workflows/deploy.yml/badge.svg?branch=main)](https://github.com/t-d-d/gh-paddock/actions/workflows/deploy.yml)
[![Licence: Apache-2.0](https://img.shields.io/badge/licence-Apache--2.0-blue)](LICENSE)

GitHub-hosted runners bill private repos per minute, rounded up per job, and start every job on a fresh machine. gh-paddock runs those jobs on Fargate tasks in an AWS account of their own instead, billed per second.

One listener task holds a message session on GitHub's scale-set API for each scale set it serves. Each response says how many jobs are assigned to that set. The listener starts a runner task for each job its runners cannot take, and hands the task a just-in-time config through Parameter Store. Every runner takes one job and deregisters. A task in a test or dev pool then starts another runner and waits 30 seconds for the next job, so a chain of stages reuses one task. The listener removes an idle runner through the API, which refuses while the runner has a job, and stops its task only after the removal succeeds.

Jobs are split into pools by the credentials they hold, and a task only runs jobs from its own pool. Public repos keep GitHub's runners: their standard minutes are free, and a fork's pull request must not reach these.

The name comes from the paddock where racehorses are kept warm before a race.

## Status

Under construction. The [v1 PRD](https://github.com/t-d-d/gh-paddock/issues/1) sets the scope: test and dev pools on Fargate Spot. Today the deploy runs a walking skeleton: a listener that only beats, a health check on that beat, a probe after each deploy, and a rollback when the probe fails.

Jobs that need a Docker daemon or KVM, macOS jobs and public repos stay on GitHub's runners.

## Layout

| Path | What |
|---|---|
| `cmd/listener/` | The listener, in Go, and its Dockerfile |
| `internal/` | The listener's packages |
| `infra/bootstrap.cfn.yml` | The deploy role GitHub's OIDC assumes, and the App key's secret |
| `infra/paddock.cfn.yml` | The listener's service, its alarm and the alerts topic |
| `scripts/` | The deploy's probe and stack guard, and the public-repo scan |
| `test/` | Tests of the stacks, the workflows and the scripts |
| `docs/constraints/` | The rules in force |

`npm run gate` runs every check offline: the public-repo scan, lint, types and the Node tests, then `gofmt`, `go vet` and the Go tests.

## Deploying your own

The deploy needs an AWS account that holds gh-paddock alone. Apply `infra/bootstrap.cfn.yml` once as an administrator, put its `DeployRoleArn` output in the repository secret `AWS_DEPLOY_ROLE_ARN`, set the repository variable `AWS_REGION`, and write the alert address to the Parameter Store parameter `/paddock/alert-email`. From then on every green merge to main deploys. The operator guide comes with v1.

## Licence

[Apache-2.0](LICENSE).
