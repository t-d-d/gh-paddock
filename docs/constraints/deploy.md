# The deploy
Relevance: How the stacks are applied and rolled back: .github/workflows/deploy.yml, .github/actions/**, infra/**, scripts/probe.mjs, scripts/cfn-continue-stalled-rollback.mjs, the listener's health check
Decision: Only main deploys, after a green gate; a red probe rolls the stack back to both images it ran before, and a failed rollback pages the operator.

Why: Every private repo's Linux jobs wait on the listener. A bad image left
running stops them all, and nobody watches the deploy as it happens.

## Rules

- `deploy.yml` runs on a push to main and by hand on main, after the gate
  passes, and only once the `AWS_DEPLOY_ROLE_ARN` secret is set. The deploy
  role's trust pins `refs/heads/main`, so a run on any other branch cannot
  assume it. Nothing else applies a stack, and no person applies one from
  a laptop once the bootstrap stack exists.
- Deploys run one at a time, and a running deploy is never cancelled.
  GitHub keeps one pending run per concurrency group and cancels the older
  when another queues; a newer push carries the older commit, so that
  loses nothing. A probe-only run queues in its own group, so it never
  takes a push's place.
- The deploy applies `paddock-bootstrap` before `paddock`, and runs
  scripts/cfn-continue-stalled-rollback.mjs before each. Before the main
  stack it makes sure ECS's service-linked role exists: a cluster with
  capacity providers can fail its first create while ECS makes the role.
- The listener's image tag is a hash of exactly the files the listener's
  Dockerfile copies, and the Dockerfile. An unchanged listener reuses its
  image; the registry's tags are immutable.
- The runner's image is built in a job per architecture, x64 and arm64,
  each on a GitHub-hosted runner of that architecture, under a tag that
  hashes runner/ with the architecture after it. runner/'s Dockerfile copies
  nothing from the repo. A job of its own joins the two under one image
  index, converts that into an index with a SOCI index manifest v2 for each
  architecture at the bare tag, and hands the deploy job the converted
  index's digest alone. It refuses an index without a SOCI index for each
  architecture. The soci CLI that converts it, and crane, which pushes it
  with every manifest's bytes unchanged, are each one release by number,
  checked against a pinned sha256 before it is unpacked.
  The stack takes it as a second parameter, `RunnerImageUri`, so both
  architectures roll back together as one image.
- The listener's service stops the old task before it starts the new one
  (minimum healthy 0%, maximum 100%): GitHub allows one message session per
  scale set. Jobs wait in GitHub for the minute between.
- The listener is healthy only while its heartbeat is fresh
  (`listener -healthcheck`). Its first beat follows the App's check with
  GitHub, and each later one a successful poll, so a task with a bad key
  never turns healthy.
- Two layers roll back. A new task that never turns healthy trips the
  service's circuit breaker: ECS returns to the last good task definition,
  the apply fails, and CloudFormation rolls the stack back. A red probe
  after a green apply rolls back in the deploy: it re-applies the same
  template with both images the stack ran before the apply, changing
  nothing else, and probes again. A stack that ran no runner image before
  keeps the one just built.
- The probe (scripts/probe.mjs) passes once the service's primary
  deployment runs the expected image, has finished rolling out, and a task
  of it reports healthy. It fails at once on a failed rollout or another
  image, waits through a failed read of the service, and fails when its
  ten minutes run out. It names images by digest only.
- A rollback whose apply or probe fails, or is cancelled when the job runs
  out of time, pages the operator through the stack's `AlertsTopic`. The
  deploy stays red after any rollback.
- A probe-only run applies nothing and rolls nothing back.

## Examples

- The `fitness: … (deploy.md)` suites in test/deploy-workflow.test.ts hold
  the order, the rollback's conditions, both images' tags, and the runner's
  index of both architectures.
- test/stack.test.ts holds the circuit breaker, the old task stopping first
  and the health check, and test/probe.test.ts the probe's judgment and
  exit code.
- The gate's image job starts the listener as ECS does and checks its health
  check stays red with no beat (test/gate-workflow.test.ts).

## Do not

- Do not roll back on a red probe when neither image changed, or on a
  probe-only run: the rollback would re-apply what is already running.
- Do not start the new listener beside the old one: GitHub refuses the new
  one's sessions until the old one stops.
- Do not build either architecture's runner image under emulation: it
  triples the build. Each builds on a runner of its own architecture.
- Do not let a deploy cancel one in progress: a cancelled apply can leave
  the stack mid-update.
- Do not hash a file list by hand that drifts from the Dockerfile: a file
  copied but not hashed ships under an old tag.

Judgment: whether the probe proves enough of the listener's work grows with the listener; the tests pin only what it checks today. A healthy task has reached GitHub with the App key, but the probe sees nothing of a runner task, so a broken runner image passes it.
History: Set with the repo's scaffold, November 2026, under PRD #1. Modelled on gh-governor's deploy. The old task stopping first, the runner's image and the two-image rollback came with #16, settled in PRD #1. #70 built the runner's image for x64 and arm64 and joined them under one index. #51 added a SOCI index for each architecture, so Fargate lazy loads the runner's image.
