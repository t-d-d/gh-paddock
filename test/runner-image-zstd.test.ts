/**
 * A cold runner task pulls and unpacks the runner's image before its
 * container starts. Fargate pulls and unpacks zstd layers faster than gzip,
 * so the deploy pushes the runner's image with every layer, its base's
 * included, recompressed as zstd. The listener's image is 5 MB and keeps
 * gzip.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { REPO_ROOT } from './helpers/cfn.ts';
import { loadWorkflow, stepById, stepsOf, type WorkflowStep } from './helpers/workflow.ts';

const { workflow } = loadWorkflow('.github/workflows/deploy.yml');
const { workflow: action } = loadWorkflow('.github/actions/image/action.yml');
const actionSteps: WorkflowStep[] = action.runs?.steps ?? [];
const build = actionSteps.find((s) => s.uses?.startsWith('docker/build-push-action@'));

describe('the runner\'s image ships with zstd layers', () => {
  test('the deploy asks for zstd for the runner\'s image alone', () => {
    assert.equal(stepById(stepsOf(workflow, 'runner-build'), 'image').with?.compression, 'zstd');
    assert.equal(stepById(stepsOf(workflow, 'deploy'), 'image').with?.compression, undefined);
  });

  test('the image action takes a compression, empty by default, which leaves the push as it was', () => {
    assert.equal(action.inputs?.compression?.default, '');
    assert.ok(build, 'the action builds with docker/build-push-action');
    assert.equal(build.with?.push, '${{ inputs.compression == \'\' }}');
  });

  // The value lands inside buildx's output string, where a comma would add
  // exporter options of its own, such as push=false.
  test('the image action refuses a compression buildx does not name, before it builds', () => {
    const checkAt = actionSteps.findIndex((s) => s.id === 'compression');
    assert.ok(checkAt >= 0, 'the action checks its compression');
    assert.ok(checkAt < actionSteps.indexOf(build!), 'the check runs before the build');
    const check = stepById(actionSteps, 'compression');
    assert.equal(check.if, undefined, 'the check runs whether or not the tag exists');
    assert.equal(check.env?.COMPRESSION, '${{ inputs.compression }}');
    const run = (value: string) => spawnSync('bash', ['-e', '-c', check.run ?? ''], { env: { ...process.env, COMPRESSION: value } }).status;
    for (const ok of ['', 'gzip', 'zstd', 'estargz', 'uncompressed']) assert.equal(run(ok), 0, `refused ${ok}`);
    for (const bad of ['zstd,push=false', 'lz4', 'ZSTD']) assert.notEqual(run(bad), 0, `took ${bad}`);
  });

  // Without force-compression buildx pushes the base's layers as GitHub
  // published them, in gzip, and they are most of the image.
  test('with a compression it recompresses every layer, the base\'s included, as OCI layers', () => {
    assert.equal(
      build?.with?.outputs,
      '${{ inputs.compression != \'\' && format(\'type=image,push=true,oci-mediatypes=true,compression={0},force-compression=true\', inputs.compression) || \'\' }}',
    );
  });

  // The registry's tags are immutable and the deploy reuses an image whose
  // tag exists, and the tag hashes runner/ alone. Only a change under
  // runner/ makes the deploy build, and so push, a zstd image.
  test('the runner\'s Dockerfile names the compression the deploy asks for, so its tag moves with the change', () => {
    const compression = stepById(stepsOf(workflow, 'runner-build'), 'image').with?.compression;
    assert.equal(typeof compression, 'string');
    const dockerfile = readFileSync(resolve(REPO_ROOT, 'runner/Dockerfile'), 'utf-8');
    // The comment names gzip as what zstd beats, so a bare word match would
    // pass a deploy that switched to gzip with no change under runner/.
    assert.match(dockerfile, new RegExp(`^# .*\\brecompressed as ${compression}\\b`, 'm'));
  });
});
