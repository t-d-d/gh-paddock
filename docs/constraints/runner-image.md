# The runner image
Relevance: runner/Dockerfile and its entrypoint, the runner's task definition (internal/runners), the gate's and the deploy's runner-image jobs, Dependabot's config
Decision: One runner image, built for x64 and arm64 FROM GitHub's runner image and kept within 30 days of its latest release. A pool runs x64 unless it sets arm64.

Why: One image keeps the runners alike and cheap to keep current. x64 is
what GitHub's own runners are, so workflows that work there work here. On
Fargate an arm64 vCPU is a whole core and costs less than an x64 one, which
is half a core, so a pool can try arm64 for the same work at a lower price. The
scale set turns runner updates off, so a runner cannot update itself
mid-job; GitHub stops sending jobs to a runner more than 30 days behind its
latest release, so the image must keep up.

## Rules

- runner/Dockerfile builds FROM `ghcr.io/actions/actions-runner:<release>`,
  a release by number, and adds only CLI tools common workflows call
  without a setup action: the AWS CLI, jq, zip, Python 3 and the libraries
  Chromium needs.
- The build installs the updates Ubuntu has published by then, in the step
  that refreshes the package lists, on both architectures, taking any new
  package an update needs. The deploy's layer cache reuses that step until
  the base or the step itself changes, so a fix Ubuntu publishes later
  reaches the image with Dependabot's next bump of the base. No build runs
  on a timer.
- It copies nothing from the repo and runs as the `runner` user. Its
  entrypoint runs a runner for each config the task is handed: `run.sh`
  reads the config and runs one job. After each job the entrypoint waits up
  to 30 seconds for a new config. It wipes the last job's checkout and
  temporary directories and keeps the caches (pool-per-repo.md), then runs
  a runner for each new one. A config it has run counts as none. It exits
  when no new config comes in the 30 seconds, or its URL is refused
  (runner-isolation.md).
- On SIGTERM a waiting entrypoint exits at once, and one whose runner has
  no job yet passes the signal on, so the runner leaves GitHub. One whose
  runner has a job never passes the signal on: the job runs until it ends
  or the task is killed, and the task then exits with no wait for another
  config. The
  task definition gives the container 120 seconds to stop, Fargate Spot's
  notice (remove-then-terminate.md).
- A runner task runs on its pool's `arch`, x64 unless the pool sets arm64
  (served-repos.md). The task definition names the architecture, and the
  image is one index of an x64 and an arm64 image, so ECS pulls the task's
  own.
- The Dockerfile names no platform and installs the AWS CLI for the
  architecture it is built on. Its base must be published for both.
- Dependabot watches the image's base weekly, with the week's cooldown every
  update gets.
- The gate builds the image for each architecture, each on a GitHub runner
  of that architecture, and starts each one's runner far enough to print
  its version.

## Examples

- `fitness: every pool runs one image, built for x64 and arm64 FROM GitHub's runner image, which Dependabot keeps current (runner-image.md)`
  in test/runner-image.test.ts, and the gate's runner-image jobs in
  test/gate-workflow.test.ts.
- The Go tests in internal/runners hold a pool's arch in its task
  definition, and that a pool with no arch runs x64.
- The entrypoint is written inline in runner/Dockerfile, and
  test/runner-entrypoint.test.ts runs it against a fake `run.sh` and a
  local config URL.

## Do not

- Do not build either architecture's image under emulation: it is slow,
  and a build on the wrong architecture can pass where the real one fails.
- Do not turn runner updates on to keep up: an update restarts the runner,
  and a single-use runner would lose its job.

Judgment: no offline check sees how far the image is behind GitHub's latest runner release. A Dependabot pull request left unmerged for three weeks is the warning.
History: Settled with the owner in PRD #1, and written by #16. #53 had the entrypoint run a runner for each config the task is handed. #54 had it wait 30 seconds after each job and exit by itself. #19 had it hold SIGTERM back from a running job. #70 let a pool run arm64, and built the image for both architectures. #100 had the build install Ubuntu's updates.
