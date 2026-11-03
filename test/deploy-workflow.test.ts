/**
 * The deploy (docs/constraints/deploy.md): a green gate on a push to main
 * applies the stacks and probes the listener, and a red probe rolls the
 * stack back to the image it ran before. Modelled on gh-governor's deploy.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { REPO_ROOT } from './helpers/cfn.ts';
import { loadWorkflow, stepById, stepsOf } from './helpers/workflow.ts';

const { workflow, codeOnly } = loadWorkflow('.github/workflows/deploy.yml');
const steps = stepsOf(workflow, 'deploy');
const indexOf = (id: string) => steps.indexOf(stepById(steps, id));

describe('fitness: only main deploys, one deploy at a time, after a green gate (deploy.md)', () => {
  test('on a push to main and by hand, never on a pull request', () => {
    assert.deepEqual(workflow.on?.push?.branches, ['main']);
    assert.ok('workflow_dispatch' in (workflow.on ?? {}));
    assert.equal(workflow.on?.pull_request, undefined);
  });

  test('after a green gate, and only once the deploy role\'s secret is set', () => {
    assert.equal(workflow.jobs?.gate?.uses, './.github/workflows/gate.yml');
    assert.deepEqual(workflow.jobs?.deploy?.needs, ['gate', 'role']);
    assert.equal(workflow.jobs?.deploy?.if, "${{ needs.role.outputs.set == 'true' }}");
  });

  // GitHub keeps one pending run per concurrency group and cancels the older
  // one when another queues. A newer push carries the older commit, so that
  // loss is harmless; a probe-only run must not take a push's place.
  test('one at a time: a running deploy is never cancelled, and a probe-only run queues apart', () => {
    assert.deepEqual(workflow.concurrency, {
      group: "${{ inputs.probe_only && 'deploy-probe' || 'deploy' }}",
      'cancel-in-progress': false,
    });
  });

  test('inside the deploy role\'s one-hour session, with room for an apply, a rollback and two probes', () => {
    const minutes = workflow.jobs?.deploy?.['timeout-minutes'];
    assert.ok(minutes <= 55 && minutes >= 45, `timeout-minutes is ${minutes}`);
  });
});

describe('fitness: the deploy applies the bootstrap stack, then the main stack, then probes (deploy.md)', () => {
  test('applies the bootstrap stack before the main stack', () => {
    assert.ok(indexOf('bootstrap') < indexOf('apply'));
    assert.match(stepById(steps, 'bootstrap').run ?? '', /--stack-name paddock-bootstrap/);
    assert.match(stepById(steps, 'apply').run ?? '', /--stack-name paddock\b/);
  });

  test('clears a stalled rollback before each apply', () => {
    const guards = steps.filter((s) => /cfn-continue-stalled-rollback\.mjs/.test(s.run ?? ''));
    assert.deepEqual(guards.map((s) => s.run?.trim().split(' ').pop()), ['paddock-bootstrap', 'paddock']);
    assert.ok(steps.indexOf(guards[0]!) < indexOf('bootstrap'));
    assert.ok(steps.indexOf(guards[1]!) < indexOf('apply'));
  });

  test('reads the image the stack runs before applying, applies the image it built, and probes for that image', () => {
    assert.ok(indexOf('previous') < indexOf('apply'));
    assert.ok(indexOf('apply') < indexOf('probe'));
    assert.match(stepById(steps, 'previous').run ?? '', /--stack-name paddock\b/);
    assert.equal(stepById(steps, 'apply').env?.IMAGE_URI, '${{ steps.image.outputs.image_uri }}');
    assert.match(stepById(steps, 'apply').run ?? '', /"ImageUri=\$\{IMAGE_URI\}"/);
    assert.equal(stepById(steps, 'probe').env?.EXPECTED_IMAGE_URI, '${{ steps.image.outputs.image_uri }}');
  });

  // ECS creates its service-linked role on an account's first cluster, and
  // a cluster with capacity providers can fail to attach them while it does.
  // The failed first create leaves a stack only a delete clears.
  test('makes sure ECS\'s service-linked role exists before the first apply can need it', () => {
    const run = stepById(steps, 'ecs-role').run ?? '';
    assert.match(run, /aws iam get-role --role-name AWSServiceRoleForECS/);
    assert.match(run, /aws iam create-service-linked-role --aws-service-name ecs\.amazonaws\.com/);
    assert.equal(stepById(steps, 'ecs-role').if, '${{ !inputs.probe_only }}');
    assert.ok(indexOf('ecs-role') < indexOf('apply'));
  });

  test('a probe-only run applies nothing', () => {
    const guards = steps.filter((s) => /cfn-continue-stalled-rollback\.mjs/.test(s.run ?? ''));
    for (const step of [...['bootstrap', 'plan', 'image', 'network', 'ecs-role', 'previous', 'apply'].map((id) => stepById(steps, id)), ...guards]) {
      assert.equal(step.if, '${{ !inputs.probe_only }}', `${step.id ?? step.name} runs on a probe-only run`);
    }
    assert.equal(stepById(steps, 'probe').if, undefined);
  });
});

describe('fitness: a red probe rolls the stack back to the image it ran before, and a failed rollback pages (deploy.md)', () => {
  const rollback = stepById(steps, 'rollback');
  const reprobe = stepById(steps, 'reprobe');
  const page = stepById(steps, 'page');

  test('rolls back only after a red probe that changed the image, and never on a probe-only run', () => {
    assert.equal(
      rollback.if,
      "${{ failure() && !inputs.probe_only && steps.probe.outcome == 'failure' && steps.previous.outputs.image_uri != '' && steps.previous.outputs.image_uri != steps.image.outputs.image_uri }}",
    );
  });

  test('re-applies the same template with the image before, changing nothing else', () => {
    const run = rollback.run ?? '';
    assert.match(run, /--stack-name paddock\b/);
    assert.match(run, /--template-file infra\/paddock\.cfn\.yml/);
    assert.deepEqual([...run.matchAll(/"(\w+)=/g)].map((m) => m[1]), ['ImageUri']);
    assert.equal(rollback.env?.PREVIOUS_IMAGE_URI, '${{ steps.previous.outputs.image_uri }}');
  });

  test('probes the image it rolled back to', () => {
    assert.equal(reprobe.if, "${{ failure() && steps.rollback.outcome == 'success' }}");
    assert.equal(reprobe.env?.EXPECTED_IMAGE_URI, '${{ steps.previous.outputs.image_uri }}');
    assert.match(reprobe.run ?? '', /node scripts\/probe\.mjs/);
  });

  // A job that reaches its timeout is cancelled, not failed, and the
  // listener's state is then unknown.
  test('pages the operator when the rollback does not bring the listener back, or runs out of time', () => {
    assert.equal(
      page.if,
      "${{ (failure() || cancelled()) && (steps.rollback.outcome == 'failure' || steps.rollback.outcome == 'cancelled' || steps.reprobe.outcome == 'failure' || steps.reprobe.outcome == 'cancelled') }}",
    );
    assert.match(page.run ?? '', /--stack-name paddock --logical-resource-id AlertsTopic/);
    assert.match(page.run ?? '', /aws sns publish/);
  });
});

describe('fitness: the image tag hashes exactly what the image is built from (deploy.md)', () => {
  // The tag hashes what the image is built from, so an unchanged listener
  // reuses its image and a changed one cannot. A file the Dockerfile copies
  // but the hash leaves out would ship under an old tag.
  test('hashes exactly the files the listener\'s Dockerfile copies, and the Dockerfile', () => {
    const dockerfile = readFileSync(resolve(REPO_ROOT, 'cmd/listener/Dockerfile'), 'utf-8');
    assert.doesNotMatch(dockerfile, /^ADD /m, 'an ADD would bring in a file the hash cannot see');
    const copied = [...dockerfile.matchAll(/^COPY (?!--from)(.+?) \S+$/gm)]
      .flatMap((m) => m[1]!.trim().split(/\s+/))
      .map((p) => p.replace(/\/$/, ''));
    const hashed = /git ls-tree -r HEAD -- (.+?) \|/.exec(stepById(steps, 'plan').run?.replace(/\\\n/g, ' ').replace(/\s+/g, ' ') ?? '')?.[1]?.split(' ') ?? [];
    assert.deepEqual([...hashed].sort(), [...copied, 'cmd/listener/Dockerfile'].sort());
  });

  test('builds the listener\'s image from its Dockerfile', () => {
    const image = stepById(steps, 'image');
    assert.equal(image.uses, './.github/actions/image');
    assert.equal(image.with?.dockerfile, 'cmd/listener/Dockerfile');
    assert.equal(image.with?.image_tag, '${{ steps.plan.outputs.image_tag }}');
  });
});

describe('the deploy\'s settings', () => {
  test('reads AWS_REGION alone from the repository variables', () => {
    const variables = [...new Set([...codeOnly.matchAll(/\bvars\.([A-Za-z_]+)/g)].map((m) => m[1]))].sort();
    assert.deepEqual(variables, ['AWS_REGION']);
  });
});
