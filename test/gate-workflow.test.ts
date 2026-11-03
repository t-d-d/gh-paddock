/**
 * The gate (CLAUDE.md, The gate): every push and every fork's pull request
 * runs `npm run gate` and builds the listener's image, and no workflow here
 * runs on the runners this repo deploys (docs/constraints/public-repo.md).
 *
 * A branch pushed here is gated by its push run. Its pull request must carry
 * no other `gate` or `image` check: a skipped job reports success, so a
 * skipped `gate` would read as green while the push run's is still running.
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
const FORK_ONLY = 'github.event.pull_request.head.repo.full_name != github.repository';
const GATE_CHECKS = ['gate', 'image'];

const workflowFiles = readdirSync(resolve(REPO_ROOT, '.github/workflows')).filter((f) => /\.ya?ml$/.test(f));
const workflows = workflowFiles.map((file) => ({ file, ...loadWorkflow(`.github/workflows/${file}`) }));
const triggers = (w: Record<string, any>): string[] =>
  typeof w.on === 'string' ? [w.on] : Array.isArray(w.on) ? w.on : Object.keys(w.on ?? {});

describe('the gate', () => {
  test('runs on every branch push, and when another workflow calls it, never on a pull request of its own', () => {
    assert.deepEqual(workflow.on?.push?.['branches-ignore'], ['main']);
    assert.ok('workflow_call' in (workflow.on ?? {}));
    assert.deepEqual(triggers(workflow).sort(), ['push', 'workflow_call']);
  });

  test('its jobs run their steps whenever it runs: neither carries an `if` that could skip it', () => {
    for (const job of GATE_CHECKS) assert.equal(workflow.jobs?.[job]?.if, undefined, job);
  });

  // A skipped job finishes at once and reports success, and a required check
  // passes on it. A pull_request run's `gate` beside the push run's would let
  // a reader of the head's checks merge before the push run's gate finished.
  test('no workflow a pull request runs has a job whose check is named `gate` or `image`', () => {
    for (const { file, workflow: w } of workflows) {
      if (!triggers(w).some((t) => t.startsWith('pull_request'))) continue;
      for (const [id, job] of Object.entries(w.jobs ?? {}) as [string, { name?: string }][]) {
        assert.ok(!GATE_CHECKS.includes(job.name ?? id), `${file} job ${id} reports a check named ${job.name ?? id}`);
      }
    }
  });

  // The fork's run calls the gate, so its checks are `<job> / gate` and
  // `<job> / image`, and the steps live in one place.
  test('a fork\'s pull request is gated by a pull_request run that calls the gate, and only a fork\'s', () => {
    const callers = workflows.flatMap(({ file, workflow: w }) =>
      (Object.entries(w.jobs ?? {}) as [string, { uses?: string; if?: string }][])
        .filter(([, job]) => job.uses === './.github/workflows/gate.yml')
        .map(([id, job]) => ({ file, w, id, job })),
    );
    const forks = callers.filter(({ w }) => triggers(w).includes('pull_request'));
    assert.equal(forks.length, 1, `callers on pull_request: ${forks.map((c) => `${c.file} ${c.id}`).join(', ')}`);
    const fork = forks[0]!;
    assert.deepEqual(triggers(fork.w), ['pull_request'], `${fork.file} runs on pull_request alone, never pull_request_target`);
    assert.equal(fork.job.if, FORK_ONLY);
    assert.deepEqual(fork.w.permissions, { contents: 'read' });
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
  test('every job names a GitHub-hosted Ubuntu runner, or calls another workflow', () => {
    assert.ok(workflows.length > 0);
    for (const { file, workflow: w } of workflows) {
      const jobs = w.jobs ?? {};
      for (const [name, job] of Object.entries(jobs) as [string, { 'runs-on'?: unknown; uses?: string }][]) {
        if (job.uses) continue;
        assert.match(String(job['runs-on']), /^ubuntu-(latest|\d{2}\.\d{2}(-arm)?)$/, `${file} ${name} runs on ${JSON.stringify(job['runs-on'])}`);
      }
    }
  });
});
