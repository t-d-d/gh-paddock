# The runner image
Relevance: runner/Dockerfile and its entrypoint, the runner's task definition (internal/runners), the gate's and the deploy's runner-image jobs, Dependabot's config
Decision: Every pool runs x64 in v1, from one runner image built FROM GitHub's runner image and kept within 30 days of its latest release.

Why: One image keeps the runners alike and cheap to keep current. x64 is
what GitHub's own runners are, so workflows that work there work here. The
scale set turns runner updates off, so a runner cannot update itself
mid-job; GitHub stops sending jobs to a runner more than 30 days behind its
latest release, so the image must keep up.

## Rules

- runner/Dockerfile builds FROM `ghcr.io/actions/actions-runner:<release>`,
  a release by number, and adds only CLI tools common workflows call
  without a setup action: the AWS CLI, jq, zip, Python 3 and the libraries
  Chromium needs.
- It copies nothing from the repo and runs as the `runner` user. Its
  entrypoint runs a runner for each config the task is handed: `run.sh`
  reads the config and runs one job. After each job the entrypoint waits up
  to 30 seconds for a new config. It wipes the last job's checkout and
  temporary directories and keeps the caches (pool-per-repo.md), then runs
  a runner for each new one. A config it has run counts as none. It exits
  when no new config comes in the 30 seconds, or its URL is refused
  (runner-isolation.md).
- Every runner task runs x64.
- Dependabot watches the image's base weekly, with the week's cooldown every
  update gets.
- The gate builds the image on x64 and starts its runner far enough to print
  its version.

## Examples

- `fitness: every pool runs one x64 image, FROM GitHub's runner image, which Dependabot keeps current (runner-image.md)`
  in test/runner-image.test.ts, and the gate's runner-image job in
  test/gate-workflow.test.ts.
- The entrypoint is written inline in runner/Dockerfile, and
  test/runner-entrypoint.test.ts runs it against a fake `run.sh` and a
  local config URL.

## Do not

- Do not build the image on arm64 with emulation: it is slow, and the runners
  are x64.
- Do not turn runner updates on to keep up: an update restarts the runner,
  and a single-use runner would lose its job.

Judgment: no offline check sees how far the image is behind GitHub's latest runner release. A Dependabot pull request left unmerged for three weeks is the warning.
History: Settled with the owner in PRD #1, and written by #16. #53 had the entrypoint run a runner for each config the task is handed. #54 had it wait 30 seconds after each job and exit by itself.
