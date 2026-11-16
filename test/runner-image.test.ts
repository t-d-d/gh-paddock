/**
 * The runner's image, runner/Dockerfile (docs/constraints/runner-image.md):
 * one image for every pool, built for x64 and arm64 from one Dockerfile,
 * FROM GitHub's official runner image, with the CLI tools common workflows
 * call without a setup action, and kept current by Dependabot.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import yaml from 'js-yaml';
import { REPO_ROOT } from './helpers/cfn.ts';

const dockerfile = readFileSync(resolve(REPO_ROOT, 'runner/Dockerfile'), 'utf-8');
const froms = [...dockerfile.matchAll(/^FROM\s+(\S+)/gm)].map((m) => m[1]);

describe('fitness: every pool runs one image, built for x64 and arm64 FROM GitHub\'s runner image, which Dependabot keeps current (runner-image.md)', () => {
  test('builds FROM GitHub\'s official runner image, at a release Dependabot can move', () => {
    assert.equal(froms.length, 1, froms.join(', '));
    assert.match(froms[0] ?? '', /^ghcr\.io\/actions\/actions-runner:\d+\.\d+\.\d+$/);
  });

  // GitHub publishes the base for both architectures, so the builder's own
  // architecture picks its layer. A pinned platform would build one
  // architecture's image under the other's name.
  test('names no platform, so the one Dockerfile builds on either architecture', () => {
    assert.doesNotMatch(dockerfile, /--platform/);
    assert.doesNotMatch(dockerfile, /^FROM\s+--/m);
  });

  test('installs the AWS CLI for the architecture it is built on, and fails on any other', () => {
    assert.match(dockerfile, /x86_64\) cli=awscli-exe-linux-x86_64\.zip/);
    assert.match(dockerfile, /aarch64\) cli=awscli-exe-linux-aarch64\.zip/);
    assert.match(dockerfile, /case "\$\(uname -m\)" in/);
    assert.match(dockerfile, /\*\) echo "[^"]*"; exit 1/);
  });

  test('Dependabot watches the runner image\'s base, with the cooldown every update gets', () => {
    const config = yaml.load(readFileSync(resolve(REPO_ROOT, '.github/dependabot.yml'), 'utf-8')) as {
      updates?: Array<{ 'package-ecosystem'?: string; directory?: string; cooldown?: { 'default-days'?: number } }>;
    };
    const entry = (config.updates ?? []).find((u) => u['package-ecosystem'] === 'docker' && u.directory === '/runner');
    assert.ok(entry, 'no docker entry for /runner');
    assert.equal(entry?.cooldown?.['default-days'], 7);
  });

  test('carries the AWS CLI, jq, zip, Python 3 and the libraries Chromium needs', () => {
    for (const want of ['awscli', 'jq', 'zip', 'python3', 'libnss3', 'libgbm1', 'libasound2']) {
      assert.ok(dockerfile.includes(want), want);
    }
  });

  // How the image runs a runner for each config the task is handed is the
  // entrypoint's to hold, and its own check comes with it.
  test('runs as the runner\'s own user, and starts from the runner\'s home', () => {
    const users = [...dockerfile.matchAll(/^USER\s+(\S+)/gm)].map((m) => m[1]);
    assert.equal(users.at(-1), 'runner');
    assert.match(dockerfile, /^(?:CMD|ENTRYPOINT) \["\/home\/runner\/[^"]+"/m);
  });
});
