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
| `infra/bootstrap.cfn.yml` | The deploy role GitHub's OIDC assumes, and the registry's scan-on-push rule |
| `infra/paddock.cfn.yml` | The listener's service, what it needs to start runner tasks, its alarms and the alerts topic |
| `runner/` | The runner's image, built FROM GitHub's runner image |
| `scripts/` | The deploy's probe and stack guard, and the public-repo scan |
| `test/` | Tests of the stacks, the workflows and the scripts |
| `docs/constraints/` | The rules in force |
| `docs/pages/` | The runner lane page, published on GitHub Pages |

`npm run gate` runs every check offline: the public-repo scan, lint, types and the Node tests, then `gofmt`, `go vet` and the Go tests.

## Deploying your own

This guide takes you from a fork to a first job on your own runners, in a fresh AWS account and GitHub org. Take the steps in order. Every name below is an example: `acme` is a GitHub org, `acme/widgets` a private repo in it, and 123456789012 is AWS's example account id.

You need:

- an AWS account that will hold gh-paddock and nothing else, and an administrator's credentials for it on your machine, with the AWS CLI and `jq`;
- a GitHub org that owns the private repos you want to serve. The runners serve private repos in an org, never public repos or repos in a personal account.
- that org on the GitHub Team plan, or in GitHub Enterprise Cloud. The listener makes a runner group for each pool, and GitHub lets an org make runner groups beyond its default one only on those plans. On GitHub Free the listener cannot make the groups, and logs each repo as `repo skipped`.

Everything in AWS lives in us-east-1. Run each `aws` command below with `--region us-east-1`, or set `AWS_REGION=us-east-1` first.

### Fork the repo

1. Fork this repo. Keep its name and `main` as its default branch. Only a run on `main` can deploy.
2. In the fork, open the Actions tab and turn workflows on. GitHub turns them off in a new fork.
3. Disable the heartbeat and config check workflows in the fork's Actions tab. They check this repo's own governor, which your fork does not have, so heartbeat would fail every hour.

Until you set the deploy's secret below, a push to `main` runs the gate and skips the deploy. The fork's own workflows run on GitHub's runners, never on the runners they deploy, because a pull request from a fork of yours must never reach your AWS account.

### Check the AWS account

1. The account needs a default VPC in us-east-1. The listener and the runners run in its public subnets, with no NAT gateway to pay for. If it has none, run `aws ec2 create-default-vpc`.
2. It must not have an IAM OIDC provider for `token.actions.githubusercontent.com` yet. The bootstrap stack creates one, and the apply fails if one exists.
   The bootstrap stack also sets the registry's scanning configuration in us-east-1: basic scanning, with images in the `paddock-` repositories scanned on push. It replaces any scanning rules the registry already has.
3. Raise the Fargate vCPU quotas. Open Service Quotas, then AWS services, then Amazon Elastic Container Service, and find `Fargate On-Demand vCPU resource count` and `Fargate Spot vCPU resource count`. A new account's quotas can be far below what a pool needs, and a raise can take a day, so ask now. Each needs the sum, over every pool on that capacity, of `maxRunners` times `vcpu`, which is 20 for a pool left at the defaults, plus a quarter of a vCPU of Spot for the listener.

### Create the GitHub App

The listener acts in GitHub as an App of your own.

1. In the org's settings, open Developer settings, then GitHub Apps, and choose New GitHub App. A private App can only be installed on the account that owns it, so create it in the org whose repos it serves. To serve repos in more than one org, let it be installed on any account.
2. Give it any name and homepage URL. Clear Active under Webhook. The listener polls GitHub and takes no webhooks.
3. Give it two permissions and no others:
   - Organization permissions, Self-hosted runners: Read and write. The listener makes a scale set and a runner group for each pool, and deletes both when the pool leaves the list.
   - Repository permissions, Actions: Read and write. The listener uses it only to re-run a job that a Spot reclaim killed.
4. Create the App, and note its App ID from its settings page.
5. Under Private keys, choose Generate a private key. GitHub downloads a `.pem` file.
6. Under Install App, install it on the org. If you choose only select repositories, add each repo you will serve, the scratch repo below included.

### Write the App's key

The listener reads the App's id and key from the Parameter Store parameter `/paddock/github-app`, a SecureString under the account's default key. No stack creates it, so the key never passes through GitHub or a CloudFormation template, and only the listener reads it.

Its value is one JSON object. `appId` is the App ID as a JSON number, not a string. `privateKey` is the PEM exactly as GitHub gave it, in PKCS#1 form, with its line breaks as `\n` escapes and its trailing line break kept. `jq` builds it:

```sh
aws ssm put-parameter --name /paddock/github-app --type SecureString \
  --value "$(jq -n --argjson id 123456 --rawfile key acme-paddock.private-key.pem '{appId: $id, privateKey: $key}')"
```

The value looks like `{"appId": 123456, "privateKey": "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----\n"}`. Delete the `.pem` file once the parameter holds it.

### Write the alert address

Alarms email one address, read from the Parameter Store parameter `/paddock/alert-email`. Write it as a String, not a SecureString. The main stack reads it at deploy, and CloudFormation cannot read a SecureString there. It lives in the account so it never shows in the fork's public run logs.

```sh
aws ssm put-parameter --name /paddock/alert-email --type String --value ops@example.com
```

### Apply the bootstrap stack

`infra/bootstrap.cfn.yml` makes GitHub's OIDC provider and the role a run on the fork's `main` assumes to deploy, and sets the registry's scan-on-push rule for the `paddock-` repositories. Nothing can assume a role that does not exist yet, so you apply it once by hand. Every deploy applies it again after that.

It takes four parameters, which pin the role to your fork by name and by numeric id, so a repo that later takes either name cannot assume it:

- `GitHubOwner`: the fork's owner, by name.
- `GitHubOwnerId`: the owner's numeric id, the `id` in `https://api.github.com/users/<owner>`.
- `GitHubRepo`: the fork's name, without the owner.
- `GitHubRepoId`: the fork's numeric id, the `id` in `https://api.github.com/repos/<owner>/<repo>`.

From a clone of the fork:

```sh
aws cloudformation deploy --stack-name paddock-bootstrap \
  --template-file infra/bootstrap.cfn.yml --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides GitHubOwner=acme GitHubOwnerId=1234567 \
    GitHubRepo=gh-paddock GitHubRepoId=7654321
aws cloudformation describe-stacks --stack-name paddock-bootstrap \
  --query "Stacks[0].Outputs[?OutputKey=='DeployRoleArn'].OutputValue" --output text
```

The role holds AdministratorAccess, because it creates the main stack's roles and passes them to ECS. That is why the account holds gh-paddock alone.

### Set the deploy's secret

The fork needs one secret and no repository variables. In the fork's settings, open Secrets and variables, then Actions, and add the repository secret `AWS_DEPLOY_ROLE_ARN` with the bootstrap stack's `DeployRoleArn` output, such as `arn:aws:iam::123456789012:role/paddock-github-deploy`. It is a secret because it carries the account id, which a public run log must not show.

### Deploy

Open the fork's Actions tab, choose the Deploy workflow and run it on `main`. From then on every green push to `main` deploys, as does a run by hand on `main`, and nothing else does.

The deploy runs the gate, builds the listener's image and the runner's into ECR, applies the main stack, `infra/paddock.cfn.yml`, and probes the listener. The probe passes only once the listener has reached GitHub with the App's key, so a wrong key or App ID fails the deploy. A red probe rolls both images back to the last good ones.

Once the first deploy is green:

1. AWS emails the alert address to confirm its subscription to the topic `paddock-alerts`. Confirm it, or no alarm reaches you.
2. The listener's log is the log group `/paddock/listener`. With an empty list it logs `listener polling` each minute and serves nothing.

The Pages workflow publishes the runner lane page and fails until you turn Pages on for the fork with GitHub Actions as its source. The runners do not need it.

### Run a first job on a scratch repo

Before any real repo moves, prove the runners work on a repo nothing depends on.

1. Create a private repo in the org, such as `acme/smoke`. If the App is installed on select repositories, add this one.
2. Write the repo list, `/paddock/repos`, with this repo and one `test` pool on Spot:

   ```sh
   aws ssm put-parameter --name /paddock/repos --type String --overwrite \
     --value '{"repos": [{"repo": "acme/smoke", "pools": [{"name": "test", "capacity": "spot"}]}]}'
   ```

   The listener reads the list every two minutes, so within five its log shows `pool ready` with the pool's scale set, `paddock-smoke-test`. A `repo skipped` line says why it could not serve the repo instead, such as an org the App is not installed on. In the org's settings, under Actions, then Runner groups, the pool's runner group appears, open to that repo alone and closed to public repos.
3. Add this workflow to the scratch repo as `.github/workflows/smoke.yml`, with your fork in `repository`. It runs this repo's gate on the test pool.

   ```yaml
   name: smoke
   on: workflow_dispatch
   jobs:
     gate:
       runs-on: paddock-smoke-test
       timeout-minutes: 15
       steps:
         - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
           with:
             repository: acme/gh-paddock
         - uses: actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1 # v7.1.0
           with:
             node-version-file: .node-version
         - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
           with:
             go-version-file: go.mod
         - run: npm ci --ignore-scripts
         - run: npm run gate
   ```

4. Run the workflow by hand from the scratch repo's Actions tab. A runner task starts on Fargate Spot within about a minute, runs the job and exits. Its log is in the log group `/paddock/runners`.
5. When the job is green, take the repo off the list. Write the list without it, or `{"repos": []}`. The listener deletes the pool's scale set once its last task has stopped, and logs `scale set deleted`, then deletes its runner group and logs `runner group deleted`. Do not delete the group by hand first: the listener finds a removed pool's scale set by its runner group, so a group deleted first leaves the scale set behind. Then delete the scratch repo.

### Serve a real repo

#### The repo list

`/paddock/repos` is a String parameter in the account, never a file in the fork, because it names private repos. The listener reads it every two minutes and serves a change within five, with no restart and no deploy. Its value is one JSON object:

```json
{"repos": [
  {"repo": "acme/widgets", "pools": [
    {"name": "test", "capacity": "spot"},
    {"name": "dev", "capacity": "on-demand", "arch": "arm64", "vcpu": 4, "memoryGb": 16, "maxRunners": 5, "maxJobMinutes": 60}
  ]},
  {"repo": "acme/gadgets", "pools": [{"name": "test", "capacity": "spot"}]}
]}
```

- `repo` is `owner/name`. The App must be installed on the owner, which must be an org, and the repo must be private.
- Each pool has a `name` of lowercase letters, digits and hyphens, starting with a letter, at most 20 characters, and a `capacity`, `spot` or `on-demand`. Spot suits a pool where a red run is cheap, such as tests. On-demand suits one where a job must not be cut off, such as a deploy.
- A pool may set `arch`, `x64` or `arm64`, which defaults to `x64`. An arm64 vCPU on Fargate is a whole core and costs less than an x64 one, which is half a core, so an arm64 pool may need half the vCPUs for the same work. Move a job to an arm64 pool only once it runs on GitHub's arm runners.
- A pool may set `vcpu` and `memoryGb`, which default to 2 and 8, as GitHub's runners for private repos, and the pair must be a Fargate task size. `maxRunners` defaults to 10 and may be up to 100. More jobs than that wait in GitHub. `maxJobMinutes`, the longest a job of the pool should run, defaults to 120.
- Each pool gets a scale set and a runner group in the org, both named `paddock-<repo>-<pool>`, where `<repo>` is the repo's name in lowercase with any run of other characters as one hyphen and no hyphen at either end. A workflow's `runs-on` names it. Two repos of one org whose names fold to the same scale set cannot both be served, and the later entry is skipped. `acme/widgets`'s pools above are `paddock-widgets-test` and `paddock-widgets-dev`.
- A pool's tasks only ever run that pool's jobs, so a repo's code and credentials never share a task with another repo's.

An entry that is not valid, such as a pool with capacity `spto`, is logged as `repo list entry not valid` and skipped, and the rest of the list is served. Its repo stays as the last good list had it, so a typo never reads as a removal. A list that cannot be read or is not JSON leaves the last good list in force, or nothing at the listener's start, and changes nothing. Both raise `paddock-repo-list-unreadable` until the list reads clean. A missing or empty list serves nothing.

A changed pool setting applies to the next task the pool starts. A repo or pool taken off the list takes no new jobs, its waiting tasks exit, and its scale set is deleted once its last task stops, then its runner group. Do not delete the group by hand: the listener finds the scale set by its group, so the scale set would be left behind. A group that still holds a runner is left in place, since GitHub would move the runner to the org's default group, and the listener logs `could not delete a removed pool's runner group` on each read until the runner is gone.

#### The runner-label variables

Point a served repo's workflows at its pools through repository variables, not literal labels, so they can go back to GitHub's runners without a commit. Give each pool a variable in the served repo, such as `PADDOCK_TEST_RUNNER` and `PADDOCK_DEV_RUNNER`, and write each moved job's `runs-on` so that an empty variable falls back to the runner the job used before:

```yaml
jobs:
  test:
    runs-on: ${{ vars.PADDOCK_TEST_RUNNER || 'ubuntu-24.04' }}
```

Then add the repo to the list, wait for `pool ready`, and set each variable to its pool's scale set, such as `paddock-widgets-test`. The next push's jobs run on your runners.

A pool is the boundary between runs, so give each set of credentials its own pool, such as a `test` pool for runs that use no environment, a `dev` pool for runs in the dev environment. A run that GitHub gives fewer secrets than the pool's other runs, such as Dependabot's or a fork pull request's, must stay on GitHub's runners, so leave it out of the variable:

```yaml
    runs-on: ${{ github.actor != 'dependabot[bot]' && !github.event.pull_request.head.repo.fork && vars.PADDOCK_TEST_RUNNER || 'ubuntu-24.04' }}
```

Move only Linux jobs that need no Docker daemon, no KVM and no Android SDK. A runner task is a Fargate task, with no Docker daemon and no `/dev/kvm`. The image is GitHub's runner image plus the AWS CLI, jq, zip, Python 3 and the libraries Chromium needs. Put an arm job on an arm64 pool, and keep its fallback on arm.

#### The kill switch

To send a repo's jobs back to GitHub's runners, delete its runner-label variables. GitHub does not store a variable with an empty value. The next jobs it queues fall back to GitHub's runners, with no deploy and no change to the list. Jobs already queued on a pool stay there, so cancel and re-run them. Once gh-paddock is back, set the variables again. Use it when an alarm says jobs are waiting or the listener is down.

#### Alarms

Each alarm emails the alert address when it fires and again when it clears.

| Alarm | What it means | What to do |
|---|---|---|
| `paddock-listener-down` | The listener has not polled GitHub for five minutes. Jobs on your runners queue and nothing starts them. | Read `/paddock/listener`. If it does not recover, use the kill switch. |
| `paddock-job-waiting` | A job has waited five minutes for a runner. | The listener's log names the pool. Check its quota and `maxRunners`, or use the kill switch for that repo. |
| `paddock-failed-starts` | Three runner tasks in a row in one pool never took their jobs. The listener replaces each, but something stops runners registering: the runner image, GitHub or the network. | Read the pool's task logs in `/paddock/runners` and the listener's log. |
| `paddock-task-past-max-job` | A runner task has run 30 minutes past its pool's `maxJobMinutes`. The listener does not stop it. | The listener's log names the task. Cancel its job in GitHub, or raise the pool's `maxJobMinutes`. |
| `paddock-spot-kill-rate` | Spot reclaims killed more than 1 in 200 of a pool's jobs over 30 days. The listener re-runs a killed job once its run completes. | Move the pool's `capacity` to `on-demand` in the list. |
| `paddock-repo-list-unreadable` | The list cannot be read or parsed, or an entry in it is not valid. The last good list stays in force. | The listener's log gives the error. Fix `/paddock/repos`. |

A deploy whose rollback fails after a red probe emails the same address, because jobs then queue with nothing to start them.

### Cost per repo

Of the tags the listener puts on every runner task it starts, two say whose cost it is. `paddock:repo` holds the repo as `owner/name`, and `paddock:pool` holds the pool's name. Cost Explorer can split the tasks' Fargate cost by either key once both are cost allocation tags. To turn them on:

1. Sign in to the organisation's management account. If the account is in an AWS Organization, only that account can turn on cost allocation tags. A standalone account is its own management account.
2. Open Billing and Cost Management, then Cost allocation tags, and find `paddock:repo` and `paddock:pool` under the user-defined tags. A key shows there only after a task carrying it has run, which can take up to 24 hours.
3. Select both keys and choose Activate. AWS takes up to 24 hours to apply them.

To read cost per repo, open Cost Explorer, filter Service to EC2 Container Service, which is how Cost Explorer names ECS. Group by the tag `paddock:repo`. To split one repo's cost by pool, filter that tag to the repo and group by `paddock:pool`. The listener's own task, the listener's and the runners' logs, Parameter Store, the runner-config bucket and the tasks' public IPv4 addresses carry neither tag, so they show as cost of no repo.

### Upgrading

Merge this repo's `main` into your fork's `main`. The green push deploys it. While the new listener starts, queued jobs wait in GitHub for about a minute and running jobs carry on.

## Licence

[Apache-2.0](LICENSE).
