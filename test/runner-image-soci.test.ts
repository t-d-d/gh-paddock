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

// soci converts the index, and crane pushes it: crane writes every manifest's
// bytes as they are on disk, where skopeo re-serializes the index and refuses
// to push it when that would change it. scripts/soci/install.sh installs both,
// each one release by number, checked against its sha256 before it is
// unpacked. The deploy's join job and the gate's soci job both run it.
const INSTALL = 'scripts/soci/install.sh';
const installScript = readFileSync(resolve(REPO_ROOT, INSTALL), 'utf-8');
const TOOLS = [
  { tool: 'soci', env: 'SOCI', release: 'https://github.com/awslabs/soci-snapshotter/releases/download/v${SOCI_VERSION}/' },
  { tool: 'crane', env: 'CRANE', release: 'https://github.com/google/go-containerregistry/releases/download/v${CRANE_VERSION}/' },
];

// Runs the install script with curl replaced by a stub that serves a local
// tarball holding a stub of each tool, and each tool's sha256 set by `sha`.
const runInstall = (sha: (tool: string, tarball: Buffer) => string) => {
  const dir = mkdtempSync(join(tmpdir(), 'soci-install-'));
  try {
    const bin = join(dir, 'bin');
    mkdirSync(bin);
    const env: Record<string, string> = { PATH: `${bin}:${process.env.PATH}` };
    const cases: string[] = [];
    for (const { tool, env: name } of TOOLS) {
      const src = join(dir, `src-${tool}`);
      mkdirSync(src);
      writeFileSync(join(src, tool), `#!/bin/sh\necho ${tool}\n`, { mode: 0o755 });
      writeFileSync(join(src, 'NOTICE.md'), 'notice\n');
      const tarball = join(dir, `${tool}.tar.gz`);
      assert.equal(spawnSync('tar', ['-czf', tarball, '-C', src, tool, 'NOTICE.md']).status, 0);
      env[`${name}_SHA256`] = sha(tool, readFileSync(tarball));
      cases.push(`*${tool === 'soci' ? 'soci-snapshotter' : 'go-containerregistry'}*) cp "${tarball}" "$out" ;;`);
    }
    writeFileSync(join(bin, 'curl'), `#!/bin/bash
out=""; url=""; while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift ;; http*) url="$1" ;; esac; shift; done
case "$url" in
${cases.join('\n')}
  *) exit 22 ;;
esac
`, { mode: 0o755 });
    const target = join(dir, 'tools');
    const result = spawnSync('bash', [resolve(REPO_ROOT, INSTALL), target], { env, stdio: 'pipe' });
    const installed = TOOLS.filter(({ tool }) => existsSync(join(target, tool))).map(({ tool }) => tool);
    return { status: result.status, installed, stderr: result.stderr.toString() };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
};
const matching = (_tool: string, b: Buffer) => createHash('sha256').update(b).digest('hex');

describe('fitness: the deploy installs one pinned release of soci and of crane, checked before it runs (deploy.md)', () => {
  for (const { tool, env, release } of TOOLS) {
    test(`${tool} is a release by number with its sha256 beside it, fetched from its project's releases`, () => {
      assert.match(installScript, new RegExp(`^: "\\$\\{${env}_VERSION:=\\d+\\.\\d+\\.\\d+\\}"$`, 'm'));
      assert.match(installScript, new RegExp(`^: "\\$\\{${env}_SHA256:=[0-9a-f]{64}\\}"$`, 'm'));
      assert.ok(installScript.includes(release), `${tool} comes from ${release}`);
    });
  }

  test('the deploy\'s join job installs both before its index step', () => {
    const install = indexSteps.find((s) => s.run?.includes(INSTALL));
    assert.ok(install, 'the join job runs the install script');
    assert.ok(indexSteps.indexOf(install) < indexSteps.indexOf(stepById(indexSteps, 'index')));
  });

  test('puts both on the path when each download matches its pinned sha256', () => {
    const { status, installed, stderr } = runInstall(matching);
    assert.equal(status, 0, stderr);
    assert.deepEqual(installed, ['soci', 'crane']);
  });

  for (const { tool } of TOOLS) {
    test(`refuses a ${tool} download that does not match, and fails`, () => {
      const { status, installed } = runInstall((t, b) => (t === tool ? 'f'.repeat(64) : matching(t, b)));
      assert.notEqual(status, 0);
      assert.ok(!installed.includes(tool), `${tool} was installed`);
    });
  }
});
