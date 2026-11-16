/**
 * A cold runner task pulls and unpacks the runner's image before its
 * container starts, unless the image carries a SOCI index: then Fargate
 * starts the container while the rest of the image streams in. Fargate reads
 * a SOCI index manifest v2 alone, bound into the image's index by
 * `soci convert`, and only over gzip or uncompressed layers. So the deploy
 * pushes the runner's image with gzip layers and converts the index of both
 * architectures into one with a SOCI index for each. The listener's image is
 * 5 MB and keeps buildx's default push.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { REPO_ROOT } from './helpers/cfn.ts';
import { loadWorkflow, stepById, stepsOf, type WorkflowStep } from './helpers/workflow.ts';

const { workflow } = loadWorkflow('.github/workflows/deploy.yml');
const { workflow: action } = loadWorkflow('.github/actions/image/action.yml');
const actionSteps: WorkflowStep[] = action.runs?.steps ?? [];
const build = actionSteps.find((s) => s.uses?.startsWith('docker/build-push-action@'));
const indexSteps = stepsOf(workflow, 'runner-image');

describe('the runner\'s image ships with gzip layers and a SOCI index for each architecture', () => {
  test('the deploy asks for gzip for the runner\'s image alone, which SOCI reads and zstd is not', () => {
    assert.equal(stepById(stepsOf(workflow, 'runner-build'), 'image').with?.compression, 'gzip');
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

  // With a compression every layer, the base's included, is pushed as an OCI
  // layer of that compression, so the index soci converts is all OCI.
  test('with a compression it recompresses every layer, the base\'s included, as OCI layers', () => {
    assert.equal(
      build?.with?.outputs,
      '${{ inputs.compression != \'\' && format(\'type=image,push=true,oci-mediatypes=true,compression={0},force-compression=true\', inputs.compression) || \'\' }}',
    );
  });

  // The registry's tags are immutable and the deploy reuses an image whose
  // tag exists, and the tag hashes runner/ alone. Only a change under
  // runner/ makes the deploy build, and so push, a gzip image.
  test('the runner\'s Dockerfile names the compression the deploy asks for, so its tag moves with the change', () => {
    const compression = stepById(stepsOf(workflow, 'runner-build'), 'image').with?.compression;
    assert.equal(typeof compression, 'string');
    const dockerfile = readFileSync(resolve(REPO_ROOT, 'runner/Dockerfile'), 'utf-8');
    assert.match(dockerfile, new RegExp(`^# .*\\brecompressed as ${compression}\\b`, 'm'));
    assert.match(dockerfile, /^# .*\bSOCI index\b/m);
  });

  test('the join job has the time to pull both architectures\' images, convert them and push the result', () => {
    const minutes = workflow.jobs?.['runner-image']?.['timeout-minutes'];
    assert.ok(minutes >= 30 && minutes <= 45, `timeout-minutes is ${minutes}`);
    assert.equal(workflow.jobs?.['runner-image']?.['runs-on'], 'ubuntu-24.04', 'the soci binary the job fetches is for x64');
  });
});

describe('fitness: the deploy installs one pinned release of the soci CLI, checked before it runs (deploy.md)', () => {
  const install = stepById(indexSteps, 'soci');

  test('a release by number with its sha256 beside it, installed before the index step', () => {
    assert.match(String(install.env?.SOCI_VERSION), /^\d+\.\d+\.\d+$/);
    assert.match(String(install.env?.SOCI_SHA256), /^[0-9a-f]{64}$/);
    assert.ok(indexSteps.indexOf(install) < indexSteps.indexOf(stepById(indexSteps, 'index')));
    assert.match(install.run ?? '', /https:\/\/github\.com\/awslabs\/soci-snapshotter\/releases\/download\/v\$\{SOCI_VERSION\}\//);
  });

  // Runs the step with curl replaced by a stub that writes a local tarball,
  // and the sha256 the step checks against set to `sha`.
  const runInstall = (sha: (tarball: Buffer) => string) => {
    const dir = mkdtempSync(join(tmpdir(), 'soci-install-'));
    try {
      const src = join(dir, 'src');
      mkdirSync(src);
      writeFileSync(join(src, 'soci'), '#!/bin/sh\necho soci\n', { mode: 0o755 });
      writeFileSync(join(src, 'NOTICE.md'), 'notice\n');
      const tarball = join(dir, 'fixture.tar.gz');
      assert.equal(spawnSync('tar', ['-czf', tarball, '-C', src, 'soci', 'NOTICE.md']).status, 0);
      const bytes = readFileSync(tarball);
      const bin = join(dir, 'bin');
      mkdirSync(bin);
      writeFileSync(join(bin, 'curl'), `#!/bin/bash
out=""; while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift ;; esac; shift; done
cp "${tarball}" "$out"
`, { mode: 0o755 });
      const runnerTemp = join(dir, 'temp');
      mkdirSync(runnerTemp);
      const githubPath = join(dir, 'path');
      const result = spawnSync('bash', ['-e', '-c', install.run ?? ''], {
        env: { PATH: `${bin}:${process.env.PATH}`, RUNNER_TEMP: runnerTemp, GITHUB_PATH: githubPath, SOCI_VERSION: String(install.env?.SOCI_VERSION), SOCI_SHA256: sha(bytes) },
        stdio: 'pipe',
      });
      const added = existsSync(githubPath) ? readFileSync(githubPath, 'utf-8').trim() : '';
      return { status: result.status, added, installed: added !== '' && existsSync(join(added, 'soci')), stderr: result.stderr.toString() };
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  };

  test('puts soci on the path when the download matches the pinned sha256', () => {
    const { status, installed, stderr } = runInstall((b) => createHash('sha256').update(b).digest('hex'));
    assert.equal(status, 0, stderr);
    assert.ok(installed, 'soci is on the path');
  });

  test('refuses a download that does not match, and puts nothing on the path', () => {
    const { status, added } = runInstall(() => 'f'.repeat(64));
    assert.notEqual(status, 0);
    assert.equal(added, '');
  });
});
