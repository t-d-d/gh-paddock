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
const runnerSteps = stepsOf(workflow, 'runner-image');
const indexOf = (id: string) => steps.indexOf(stepById(steps, id));

describe('fitness: only main deploys, one deploy at a time, after a green gate (deploy.md)', () => {
  test('on a push to main and by hand, never on a pull request', () => {
    assert.deepEqual(workflow.on?.push?.branches, ['main']);
    assert.ok('workflow_dispatch' in (workflow.on ?? {}));
    assert.equal(workflow.on?.pull_request, undefined);
  });

  test('after a green gate, and only once the deploy role\'s secret is set', () => {
    assert.equal(workflow.jobs?.gate?.uses, './.github/workflows/gate.yml');
    assert.deepEqual(workflow.jobs?.['runner-image']?.needs, ['gate', 'role']);
    assert.equal(workflow.jobs?.['runner-image']?.if, "${{ needs.role.outputs.set == 'true' && !inputs.probe_only }}");
    assert.deepEqual(workflow.jobs?.deploy?.needs, ['gate', 'role', 'runner-image']);
    // A probe-only run skips the runner's image, and must still probe; any
    // other run needs the runner's image built.
    assert.equal(
      workflow.jobs?.deploy?.if,
      "${{ !cancelled() && needs.gate.result == 'success' && needs.role.outputs.set == 'true' && (inputs.probe_only || needs.runner-image.result == 'success') }}",
    );
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
    assert.equal(stepById(steps, 'apply').env?.RUNNER_IMAGE_URI, '${{ steps.runner.outputs.image_uri }}');
    assert.match(stepById(steps, 'apply').run ?? '', /"RunnerImageUri=\$\{RUNNER_IMAGE_URI\}"/);
    assert.match(stepById(steps, 'previous').run ?? '', /read_parameter ImageUri\)[\s\S]*read_parameter RunnerImageUri\)/);
    assert.ok(indexOf('runner') < indexOf('apply'));
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
    for (const step of [...['bootstrap', 'plan', 'image', 'runner', 'network', 'ecs-role', 'previous', 'apply'].map((id) => stepById(steps, id)), ...guards]) {
      assert.equal(step.if, '${{ !inputs.probe_only }}', `${step.id ?? step.name} runs on a probe-only run`);
    }
    assert.equal(stepById(steps, 'probe').if, undefined);
  });
});

describe('fitness: a red probe rolls the stack back to both images it ran before, and a failed rollback pages (deploy.md)', () => {
  const rollback = stepById(steps, 'rollback');
  const reprobe = stepById(steps, 'reprobe');
  const page = stepById(steps, 'page');

  test('rolls back only after a red probe that changed either image, and never on a probe-only run', () => {
    assert.equal(
      rollback.if,
      "${{ failure() && !inputs.probe_only && steps.probe.outcome == 'failure' && steps.previous.outputs.image_uri != '' && (steps.previous.outputs.image_uri != steps.image.outputs.image_uri || (steps.previous.outputs.runner_image_uri != '' && steps.previous.outputs.runner_image_uri != steps.runner.outputs.image_uri)) }}",
    );
  });

  test('re-applies the same template with both images before, changing nothing else', () => {
    const run = rollback.run ?? '';
    assert.match(run, /--stack-name paddock\b/);
    assert.match(run, /--template-file infra\/paddock\.cfn\.yml/);
    assert.deepEqual([...run.matchAll(/"(\w+)=/g)].map((m) => m[1]), ['ImageUri', 'RunnerImageUri']);
    assert.equal(rollback.env?.PREVIOUS_IMAGE_URI, '${{ steps.previous.outputs.image_uri }}');
    assert.equal(rollback.env?.PREVIOUS_RUNNER_IMAGE_URI, '${{ steps.previous.outputs.runner_image_uri }}');
  });

  // A stack from before the runner's image has no runner image to return
  // to; the rollback then keeps the one just built and rolls the listener
  // back alone.
  test('keeps the new runner\'s image when the stack ran none before', () => {
    const run = rollback.run ?? '';
    assert.match(run, /\$\{PREVIOUS_RUNNER_IMAGE_URI:-\$RUNNER_IMAGE_URI\}/);
    assert.equal(rollback.env?.RUNNER_IMAGE_URI, '${{ steps.runner.outputs.image_uri }}');
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

describe('fitness: the runner\'s image is built on x64 under a tag of its own, from its Dockerfile alone (deploy.md)', () => {
  test('on a GitHub-hosted x64 runner, for linux/amd64', () => {
    assert.equal(workflow.jobs?.['runner-image']?.['runs-on'], 'ubuntu-24.04');
    const image = stepById(runnerSteps, 'image');
    assert.equal(image.uses, './.github/actions/image');
    assert.equal(image.with?.dockerfile, 'runner/Dockerfile');
    assert.equal(image.with?.repo_name, 'paddock-runner');
    assert.equal(image.with?.platform, 'linux/amd64');
    assert.equal(image.with?.image_tag, '${{ steps.plan.outputs.image_tag }}');
  });

  test('its tag hashes runner/, which holds the Dockerfile and nothing it copies from elsewhere', () => {
    const dockerfile = readFileSync(resolve(REPO_ROOT, 'runner/Dockerfile'), 'utf-8');
    assert.doesNotMatch(dockerfile, /^(ADD|COPY) (?!--from)/m, 'the runner\'s image copies nothing from the repo');
    const run = stepById(runnerSteps, 'plan').run?.replace(/\\\n/g, ' ').replace(/\s+/g, ' ') ?? '';
    assert.match(run, /git ls-tree -r HEAD -- runner \|/);
  });

  // A job output that holds a masked string is dropped, and the image URI
  // carries the account id, which the credentials step masks. The job hands
  // on the digest, and the deploy job builds the URI from its own registry.
  test('hands the deploy job the digest alone, which it joins to its own registry', () => {
    assert.deepEqual(workflow.jobs?.['runner-image']?.outputs, { digest: '${{ steps.digest.outputs.digest }}' });
    const runner = stepById(steps, 'runner');
    assert.equal(runner.env?.DIGEST, '${{ needs.runner-image.outputs.digest }}');
    assert.match(runner.run ?? '', /paddock-runner@\$\{DIGEST\}/);
  });
});

describe('fitness: the deploy runs in us-east-1 alone (region.md)', () => {
  test('every AWS step names us-east-1, and no repository variable can move it', () => {
    assert.deepEqual([...codeOnly.matchAll(/\bvars\.([A-Za-z_]+)/g)].map((m) => m[1]), []);
    const regions = [...codeOnly.matchAll(/(?:aws-region|region): (\S+)/g)].map((m) => m[1]);
    assert.ok(regions.length >= 3, regions.join(', '));
    for (const region of regions) assert.equal(region, 'us-east-1');
  });
});

// Governor learns of a deploy from the deploy workflow alone
// (.github/governor.json turns deployMessages on). The job whose success
// means the change is live ends its name with the marker and names the
// commit it shipped, and the file says which merged pull requests hear of it.
describe('what governor reads of a deploy', () => {
  const name: string = workflow.jobs?.deploy?.name ?? '';

  test('the deploy job\'s name ends with the commit and the target, on a push or a run by hand', () => {
    assert.match(name, /\|\| format\('[^']* at \{0\} \u00b7 deployed: paddock', github\.sha\) \}\}$/);
  });

  test('a probe-only run deploys nothing, so its name carries no marker', () => {
    const probeOnly = /inputs\.probe_only && '([^']*)'/.exec(name);
    assert.ok(probeOnly, name);
    assert.doesNotMatch(probeOnly[1]!, /deployed:/);
  });

  test('no other job is marked: a rollback is a step of the deploy job, which then ends red', () => {
    for (const [id, job] of Object.entries(workflow.jobs ?? {}) as [string, { name?: string }][]) {
      if (id !== 'deploy') assert.doesNotMatch(job.name ?? id, /deployed:/, id);
    }
  });

  test('every pull request a deploy shipped hears of it', () => {
    assert.deepEqual(workflow.env, { TICKET_LABEL_PATHS: 'all' });
  });
});
