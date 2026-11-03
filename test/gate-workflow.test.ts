/**
 * The gate (CLAUDE.md, The gate): every push and every fork's pull request
 * runs `npm run gate` and builds the listener's image, and no workflow here
 * runs on the runners this repo deploys (docs/constraints/public-repo.md).
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { readdirSync } from 'node:fs';
import { resolve } from 'node:path';
import { REPO_ROOT } from './helpers/cfn.ts';
import { loadWorkflow, stepsOf } from './helpers/workflow.ts';

const { workflow } = loadWorkflow('.github/workflows/gate.yml');
const FORK_ONLY_ON_PR = "github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name != github.repository";

describe('the gate', () => {
  test('runs on every branch push, every pull request, and when the deploy calls it', () => {
    assert.deepEqual(workflow.on?.push?.['branches-ignore'], ['main']);
    assert.ok('pull_request' in (workflow.on ?? {}));
    assert.ok('workflow_call' in (workflow.on ?? {}));
  });

  test('gates a fork\'s pull request, and skips a branch\'s own, which its push run gates', () => {
    for (const job of ['gate', 'image']) assert.equal(workflow.jobs?.[job]?.if, FORK_ONLY_ON_PR, job);
  });

  test('runs the same `npm run gate` a contributor runs, with Go installed', () => {
    const steps = stepsOf(workflow, 'gate');
    assert.ok(steps.some((s) => s.run === 'npm run gate'));
    assert.ok(steps.some((s) => s.uses?.startsWith('actions/setup-go@')));
  });

  // ECS runs the image as its nonroot user and calls the health check inside
  // it. A heartbeat path that user cannot write, or an entrypoint that does
  // not start, would otherwise surface only after merge, as a rollback.
  test('builds the listener\'s image, starts it, and its health check passes once it beats', () => {
    const runs = stepsOf(workflow, 'image').map((s) => s.run ?? '').join('\n');
    assert.match(runs, /docker build --file cmd\/listener\/Dockerfile/);
    assert.match(runs, /docker run --detach/);
    assert.match(runs, /docker exec \S+ \/listener -healthcheck/);
  });
});

describe('fitness: this repo\'s workflows run on GitHub\'s runners, never the ones it deploys (public-repo.md)', () => {
  const dir = resolve(REPO_ROOT, '.github/workflows');
  const files = readdirSync(dir).filter((f) => /\.ya?ml$/.test(f));

  test('every job names a GitHub-hosted Ubuntu runner, or calls another workflow', () => {
    assert.ok(files.length > 0);
    for (const file of files) {
      const jobs = loadWorkflow(`.github/workflows/${file}`).workflow.jobs ?? {};
      for (const [name, job] of Object.entries(jobs) as [string, { 'runs-on'?: unknown; uses?: string }][]) {
        if (job.uses) continue;
        assert.match(String(job['runs-on']), /^ubuntu-(latest|\d{2}\.\d{2}(-arm)?)$/, `${file} ${name} runs on ${JSON.stringify(job['runs-on'])}`);
      }
    }
  });
});
