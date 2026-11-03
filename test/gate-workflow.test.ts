/**
 * The gate (CLAUDE.md, The gate): every push and every fork's pull request
 * runs `npm run gate` and builds the listener's image, and no workflow here
 * runs on the runners this repo deploys (docs/constraints/public-repo.md).
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { REPO_ROOT } from './helpers/cfn.ts';
import { loadWorkflow, stepById, stepsOf } from './helpers/workflow.ts';

const { raw, workflow } = loadWorkflow('.github/workflows/gate.yml');
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

  // The listener is built with the Dockerfile's golang tag, and Dependabot
  // moves that tag alone. A second copy of the release in the gate would let
  // `go vet` and `go test` run on one release while the image ships another.
  describe('runs Go on the release the listener\'s image builds with', () => {
    const steps = stepsOf(workflow, 'gate');
    const setupGo = steps.find((s) => s.uses?.startsWith('actions/setup-go@'));
    const ref = /^\$\{\{\s*steps\.([\w-]+)\.outputs\.([\w-]+)\s*\}\}$/.exec(String(setupGo?.with?.['go-version'] ?? ''));

    test('setup-go takes its release from an earlier step\'s output, not a literal', () => {
      assert.ok(ref, `go-version is ${JSON.stringify(setupGo?.with?.['go-version'])}`);
      assert.equal(setupGo?.with?.['go-version-file'], undefined);
      const reader = steps.findIndex((s) => s.id === ref[1]);
      assert.ok(reader >= 0 && reader < steps.indexOf(setupGo!), `step ${ref[1]} runs before setup-go`);
    });

    // Runs the reader step, as Actions runs it, against `dockerfile` placed at
    // cmd/listener/Dockerfile in a scratch directory, and returns the release
    // it outputs. Throws when the step exits non-zero.
    const readRelease = (dockerfile: string): string | undefined => {
      assert.ok(ref);
      const dir = mkdtempSync(join(tmpdir(), 'gate-go-'));
      try {
        mkdirSync(join(dir, 'cmd/listener'), { recursive: true });
        writeFileSync(join(dir, 'cmd/listener/Dockerfile'), dockerfile);
        const output = join(dir, 'output');
        execFileSync('bash', ['-eo', 'pipefail', '-c', stepById(steps, ref[1] ?? '').run ?? ''], {
          cwd: dir,
          env: { PATH: process.env.PATH, GITHUB_OUTPUT: output },
          stdio: 'pipe',
        });
        const outputs = Object.fromEntries(
          readFileSync(output, 'utf-8').trim().split('\n').map((line) => {
            const at = line.indexOf('=');
            return [line.slice(0, at), line.slice(at + 1)];
          }),
        );
        return outputs[ref[2] ?? ''];
      } finally {
        rmSync(dir, { recursive: true, force: true });
      }
    };

    test('that step reads the release from cmd/listener/Dockerfile\'s golang FROM line', () => {
      const dockerfile = readFileSync(resolve(REPO_ROOT, 'cmd/listener/Dockerfile'), 'utf-8');
      const tag = /^FROM\s.*golang:(\d+\.\d+(?:\.\d+)?)/m.exec(dockerfile)?.[1];
      assert.ok(tag, 'the Dockerfile builds FROM a golang:<release> image');
      assert.equal(readRelease(dockerfile), tag);
    });

    test('that step reads the release whatever the FROM line carries around the tag', () => {
      const cases: [string, string][] = [
        ['FROM public.ecr.aws/docker/library/golang:1.27-bookworm AS build', '1.27'],
        ['FROM golang:1.27.3-alpine AS build', '1.27.3'],
        ['FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build', '1.27'],
        ['FROM golang:1.27-bookworm@sha256:0123abcd AS build', '1.27'],
        ['FROM golang:1.27 AS build', '1.27'],
        ['FROM golang:1.27', '1.27'],
      ];
      for (const [from, release] of cases) {
        assert.equal(readRelease(`# a comment\n${from}\nRUN true\nFROM scratch\n`), release, from);
      }
    });

    test('that step fails the gate when no FROM line names a golang release', () => {
      for (const dockerfile of ['FROM scratch\n', 'FROM golang:latest\n', 'FROM golang:1.27rc1-bookworm\n']) {
        assert.throws(() => readRelease(dockerfile), JSON.stringify(dockerfile));
      }
    });

    test('the comment above setup-go says the release comes from the Dockerfile', () => {
      const lines = raw.split('\n');
      const at = lines.findIndex((l) => /uses: actions\/setup-go@/.test(l));
      let i = at - 1;
      while (i >= 0 && /^\s*#/.test(lines[i] ?? '')) i--;
      const comment = lines.slice(i + 1, at).join(' ');
      assert.match(comment, /cmd\/listener\/Dockerfile/);
      assert.match(comment, /Dependabot/);
    });
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
