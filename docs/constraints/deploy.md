# The deploy
Relevance: How the stacks are applied and rolled back: .github/workflows/deploy.yml, .github/actions/**, infra/**, scripts/probe.mjs, scripts/cfn-continue-stalled-rollback.mjs, the listener's health check
Decision: Only a green gate on main deploys; a red probe rolls the stack back to the image it ran before, and a failed rollback pages the operator.

Why: Every private repo's Linux jobs wait on the listener. A bad image left
running stops them all, and nobody watches the deploy as it happens.

## Rules

- `deploy.yml` runs on a push to main and by hand, after the gate passes,
  and only once the `AWS_DEPLOY_ROLE_ARN` secret is set. Deploys queue;
  one never cancels another. Nothing else applies a stack, and no person
  deploys by hand once the bootstrap stack exists.
- The deploy applies `paddock-bootstrap` before `paddock`, and runs
  scripts/cfn-continue-stalled-rollback.mjs before each.
- The image tag is a hash of exactly the files the listener's Dockerfile
  copies, and the Dockerfile. An unchanged listener reuses its image; the
  registry's tags are immutable.
- The listener is healthy only while its heartbeat is fresh
  (`listener -healthcheck`). Once it polls GitHub, a beat follows each
  successful poll.
- Two layers roll back. A new task that never turns healthy trips the
  service's circuit breaker: ECS returns to the last good task definition,
  the apply fails, and CloudFormation rolls the stack back. A red probe
  after a green apply rolls back in the deploy: it re-applies the same
  template with the image the stack ran before the apply, changing
  nothing else, and probes again.
- The probe (scripts/probe.mjs) passes once the service's primary
  deployment runs the expected image, has finished rolling out, and a task
  of it reports healthy. It names images by digest only.
- A rollback whose apply or probe fails pages the operator through the
  stack's `AlertsTopic`. The deploy stays red after any rollback.
- A probe-only run applies nothing and rolls nothing back.

## Examples

- test/deploy-workflow.test.ts holds the order, the rollback's conditions
  and the image tag's file list.
- test/stack.test.ts holds the circuit breaker and the health check.
- test/probe.test.ts holds the probe's judgment.

## Do not

- Do not roll back on a red probe when the image did not change, or on a
  probe-only run: the rollback would re-apply what is already running.
- Do not let a deploy cancel one in progress: a cancelled apply can leave
  the stack mid-update.
- Do not hash a file list by hand that drifts from the Dockerfile: a file
  copied but not hashed ships under an old tag.

Judgment: whether the probe proves enough of the listener's work grows with the listener; the tests pin only what it checks today.
History: Set with the repo's scaffold, November 2026, under PRD #1. Modelled on gh-governor's deploy.
