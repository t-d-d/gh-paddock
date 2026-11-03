/**
 * The gate (CLAUDE.md, The gate): every pull request, from a branch here or
 * from a fork, runs `npm run gate` and builds the listener's image, and no
 * workflow here runs on the runners this repo deploys
 * (docs/constraints/public-repo.md).
 *
 * gate.yml runs on pull_request, not on a branch push. Each commit on a pull
 * request then gets one run, and its checks are `gate` and `image` whether
 * the branch is here or on a fork, so a required `gate` check meets both. A
 * branch push would add a second run beside the pull request's, and a fork's
 * pull request has no push run here at all. main is gated by deploy.yml,
 * which calls this before it applies.
 *
 * A pull request run checks out the merge of its head into main, not the
 * head, so nothing the gate runs may depend on the head's own history.
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
const GATE_CHECKS = ['gate', 'image'];

const workflowFiles = readdirSync(resolve(REPO_ROOT, '.github/workflows')).filter((f) => /\.ya?ml$/.test(f));
const workflows = workflowFiles.map((file) => ({ file, ...loadWorkflow(`.github/workflows/${file}`) }));
const triggers = (w: Record<string, any>): string[] =>
  typeof w.on === 'string' ? [w.on] : Array.isArray(w.on) ? w.on : Object.keys(w.on ?? {});

describe('the gate', () => {
  test('runs on every pull request, and when another workflow calls it, never on a branch push', () => {
    assert.deepEqual(triggers(workflow).sort(), ['pull_request', 'workflow_call']);
    // No branch or path filter: every pull request gets its gate.
    assert.equal(workflow.on?.pull_request ?? null, null);
    assert.deepEqual(workflow.permissions, { contents: 'read' });
  });

  test('its jobs run their steps whenever it runs: neither carries an `if` that could skip it', () => {
    for (const job of GATE_CHECKS) assert.equal(workflow.jobs?.[job]?.if, undefined, job);
  });

  // A skipped job finishes at once and reports success, and a required check
  // passes on it. A second `gate` or `image` on a pull request, from another
  // workflow, could be skipped and read as green before gate.yml's finished.
  test('no other workflow a pull request runs has a job whose check is named `gate` or `image`', () => {
    for (const { file, workflow: w } of workflows) {
      if (file === 'gate.yml') continue;
      if (!triggers(w).some((t) => t.startsWith('pull_request'))) continue;
      for (const [id, job] of Object.entries(w.jobs ?? {}) as [string, { name?: string }][]) {
        assert.ok(!GATE_CHECKS.includes(job.name ?? id), `${file} job ${id} reports a check named ${job.name ?? id}`);
      }
    }
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
