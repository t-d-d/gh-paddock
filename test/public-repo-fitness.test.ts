/**
 * The repo is public (docs/constraints/public-repo.md). Every tracked file
 * passes the scan, and the deploy, whose run log anyone can read, never
 * shows the AWS account id. Adapted from gh-governor's.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { scanRepo, trackedFiles } from '../scripts/public-repo-scan.mjs';
import { loadWorkflow, stepsOf, type WorkflowStep } from './helpers/workflow.ts';

describe('fitness: no tracked file holds an id or names the served service outside a doc (public-repo.md)', () => {
  test('the scan reads every tracked file, with none exempted', () => {
    const files = trackedFiles();
    for (const path of ['scripts/public-repo-scan.mjs', 'test/public-repo-scan.test.ts', 'LICENSE', 'docs/constraints/public-repo.md', '.claude/skills/unslop/SKILL.md']) {
      assert.ok(files.includes(path), path);
    }
    const read: string[] = [];
    scanRepo({ read: (path) => { read.push(path); return ''; } });
    assert.deepEqual(read, files);
  });

  test('no tracked file has a finding', () => {
    assert.deepEqual(scanRepo().map((f) => `${f.path}:${f.line} ${f.kind} ${f.text}`), []);
  });
});

// The deploy role's ARN carries the account id, and so do the registry and
// the image URI. Actions masks a secret's exact string, and the credentials
// action masks the account id in every later line when asked to.
describe('fitness: the deploy\'s log never shows the AWS account id (public-repo.md)', () => {
  const { workflow, codeOnly } = loadWorkflow('.github/workflows/deploy.yml');
  const steps = stepsOf(workflow, 'deploy');
  const credentials = steps.filter((step) => step.uses?.startsWith('aws-actions/configure-aws-credentials@'));

  test('the deploy assumes the role named by a secret, and masks the account id', () => {
    assert.equal(credentials.length, 1);
    assert.equal(credentials[0]?.with?.['role-to-assume'], '${{ secrets.AWS_DEPLOY_ROLE_ARN }}');
    assert.equal(credentials[0]?.with?.['mask-aws-account-id'], true);
  });

  test('no step before the credentials step runs a command, and no other job holds AWS credentials', () => {
    const before = steps.slice(0, steps.indexOf(credentials[0]!));
    for (const step of before) {
      assert.equal(step.run, undefined, 'a step before the credentials step runs a command the mask does not cover');
      assert.match(step.uses ?? '', /^actions\/(checkout|setup-node)@/);
    }
    for (const [name, job] of Object.entries(workflow.jobs ?? {}) as [string, { steps?: WorkflowStep[] }][]) {
      if (name === 'deploy') continue;
      assert.ok(!(job.steps ?? []).some((s) => s.uses?.startsWith('aws-actions/configure-aws-credentials@')), `${name} holds AWS credentials`);
    }
  });

  test('the deploy\'s switch reads the secret, never a variable of the same name', () => {
    assert.doesNotMatch(codeOnly, /vars\.AWS_DEPLOY_ROLE_ARN/);
    const read = stepsOf(workflow, 'role').find((step) => step.id === 'read');
    assert.equal(read?.env?.ROLE_ARN, '${{ secrets.AWS_DEPLOY_ROLE_ARN }}');
  });
});

// A mask reaches only the log, not a run artifact or a job summary, and
// anyone signed in can download a public repo's artifacts. The build action
// uploads a build record and writes a summary by default, and both name the
// image by its registry.
describe('fitness: the image build uploads no build record and the deploy writes no summary (public-repo.md)', () => {
  const action = loadWorkflow('.github/actions/image/action.yml');
  const deploy = loadWorkflow('.github/workflows/deploy.yml');
  const actionSteps: WorkflowStep[] = action.workflow.runs?.steps ?? [];
  const off = (value: unknown) => String(value).toLowerCase() === 'false';

  // A pushed tag is never moved, so an unchanged listener keeps the image
  // its probe passed (deploy.md).
  test('creates the registry with immutable tags', () => {
    const create = actionSteps.map((s) => s.run ?? '').join('\n');
    assert.match(create, /aws ecr create-repository[\s\S]*--image-tag-mutability IMMUTABLE/);
  });

  test('every build step turns off the build record and the summary', () => {
    const builds = actionSteps.filter((s) => s.uses?.startsWith('docker/build-push-action@'));
    assert.ok(builds.length > 0);
    for (const build of builds) {
      assert.ok(off(build.env?.DOCKER_BUILD_RECORD_UPLOAD), 'the build uploads its record');
      assert.ok(off(build.env?.DOCKER_BUILD_SUMMARY), 'the build writes a summary');
    }
  });

  test('no step of the image action or the deploy writes a job summary or uploads an artifact', () => {
    for (const { codeOnly } of [action, deploy]) assert.doesNotMatch(codeOnly, /GITHUB_STEP_SUMMARY/);
    for (const step of [...actionSteps, ...stepsOf(deploy.workflow, 'deploy')]) {
      assert.doesNotMatch(step.uses ?? '', /^actions\/upload-artifact[@/]/);
    }
  });
});
