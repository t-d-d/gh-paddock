/**
 * The runner image's entrypoint, written inline in runner/Dockerfile
 * (docs/constraints/runner-image.md). It runs a runner for each config the
 * task is handed: the first from the task's parameter, each later one from
 * the presigned URL that came beside it. These tests run the script itself
 * with bash, against a fake run.sh and a config URL served on 127.0.0.1.
 */

import { test, describe, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, appendFileSync, rmSync } from 'node:fs';
import { createServer, type Server } from 'node:http';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { REPO_ROOT } from './helpers/cfn.ts';

const dockerfile = readFileSync(resolve(REPO_ROOT, 'runner/Dockerfile'), 'utf-8');
const inline = /^RUN cat > (\S+) <<'PADDOCK_ENTRYPOINT'\n([\s\S]*?)\nPADDOCK_ENTRYPOINT$/m.exec(dockerfile);
const entrypointPath = inline?.[1] ?? '';
const entrypoint = inline?.[2] ?? '';

// The fake runner logs the config it was given and what it finds of the
// last job, then plays a job: a checkout, temporary files and caches.
const fakeRun = `#!/bin/bash
set -eu
echo "run $ACTIONS_RUNNER_INPUT_JITCONFIG" >> "$FAKE_LOG"
if [ -n "\${FAKE_HOLD:-}" ]; then
  trap 'echo "term" >> "$FAKE_LOG"; exit 143' TERM
  sleep 30 & wait $!
fi
for f in _work/widgets/widgets/checkout.txt _work/_temp/temp.txt _work/_actions/a/action.yml tmp/job.txt \\
         _work/_tool/node/cached .npm/cached go/pkg/mod/cached .runner; do
  if [ -e "$FAKE_ROOT/home/$f" ] || [ -e "$FAKE_ROOT/$f" ]; then echo "  sees $f" >> "$FAKE_LOG"; fi
done
if env | grep -v '^FAKE_' | grep -q -e PADDOCK_CONFIG -e "$FAKE_URL_HOST"; then echo "  sees the URL" >> "$FAKE_LOG"; fi
mkdir -p "$HOME/_work/widgets/widgets" "$HOME/_work/_temp" "$HOME/_work/_actions/a" "$HOME/_work/_tool/node" "$HOME/.npm" "$HOME/go/pkg/mod"
touch "$HOME/_work/widgets/widgets/checkout.txt" "$HOME/_work/_temp/temp.txt" "$HOME/_work/_actions/a/action.yml" "$TMPDIR/job.txt"
touch "$HOME/_work/_tool/node/cached" "$HOME/.npm/cached" "$HOME/go/pkg/mod/cached" "$HOME/.runner"
echo "end" >> "$FAKE_LOG"
exit "\${FAKE_EXIT:-0}"
`;

type Reply = { status: number; body?: unknown };

/** A config URL that answers with `replies` in turn, then the last forever. */
class ConfigURL {
  server: Server;
  replies: Reply[] = [];
  log = '';
  requests: string[] = [];
  constructor() {
    this.server = createServer((req, res) => {
      this.requests.push(req.url ?? '');
      if (this.log) appendFileSync(this.log, 'poll\n');
      const reply = this.replies.length > 1 ? this.replies.shift()! : this.replies[0] ?? { status: 404 };
      res.statusCode = reply.status;
      res.end(reply.body === undefined ? '' : JSON.stringify(reply.body));
    });
  }
  get host() {
    const addr = this.server.address();
    return typeof addr === 'object' && addr ? `127.0.0.1:${addr.port}` : '';
  }
  get url() {
    return `http://${this.host}/configs/5/paddock-first?X-Amz-Signature=abc`;
  }
}

type Run = { code: number | null; log: string[]; seconds: number; root: string };

async function runEntrypoint(url: ConfigURL, replies: Reply[], opts: { wait?: number; exit?: number; term?: boolean } = {}): Promise<Run> {
  const root = mkdtempSync(join(tmpdir(), 'paddock-entrypoint-'));
  const home = join(root, 'home');
  mkdirSync(join(root, 'tmp'), { recursive: true });
  mkdirSync(home, { recursive: true });
  writeFileSync(join(home, 'run.sh'), fakeRun, { mode: 0o755 });
  const script = join(root, 'entrypoint.sh');
  writeFileSync(script, entrypoint, { mode: 0o755 });
  const log = join(root, 'log');
  writeFileSync(log, '');
  url.replies = replies;
  url.log = log;
  url.requests = [];
  const started = Date.now();
  const code = await new Promise<number | null>((done) => {
    const child = spawn('bash', [script], {
      cwd: home,
      stdio: 'ignore',
      // An explicit environment: no proxy reaches the local URL.
      env: {
        PATH: process.env.PATH ?? '/usr/bin:/bin',
        HOME: home,
        TMPDIR: join(root, 'tmp'),
        FAKE_LOG: log,
        FAKE_ROOT: root,
        FAKE_URL_HOST: url.host,
        FAKE_EXIT: String(opts.exit ?? 0),
        FAKE_HOLD: opts.term ? '1' : '',
        PADDOCK_CONFIG_WAIT: String(opts.wait ?? 2),
        PADDOCK_CONFIG: JSON.stringify({ runner: 'paddock-first', jitconfig: 'jit-first', url: url.url }),
      },
    });
    child.on('exit', (c) => done(c));
    // ECS stops a task with SIGTERM to its first process: send it once the
    // runner is running.
    if (opts.term) {
      const poll = setInterval(() => {
        if (readFileSync(log, 'utf-8').includes('run jit-first')) {
          clearInterval(poll);
          setTimeout(() => child.kill('SIGTERM'), 200);
        }
      }, 50);
      child.on('exit', () => clearInterval(poll));
    }
  });
  const result = { code, log: readFileSync(log, 'utf-8').split('\n').filter(Boolean), seconds: (Date.now() - started) / 1000, root };
  rmSync(root, { recursive: true, force: true });
  return result;
}

const runs = (r: Run) => r.log.filter((l) => l.startsWith('run '));
const config = (runner: string) => ({ status: 200, body: { runner, jitconfig: `jit-${runner}` } });

describe('the runner image\'s entrypoint', () => {
  const url = new ConfigURL();
  before(() => new Promise<void>((done) => url.server.listen(0, '127.0.0.1', done)));
  after(() => new Promise<void>((done) => url.server.close(() => done())));

  test('is written inline in the Dockerfile, which starts it', () => {
    assert.ok(entrypoint.startsWith('#!/bin/bash\n'), 'no inline entrypoint in runner/Dockerfile');
    assert.match(dockerfile, new RegExp(`^RUN chmod 755 ${entrypointPath}$`, 'm'));
    assert.match(dockerfile, new RegExp(`^CMD \\["${entrypointPath}"\\]$`, 'm'));
  });

  test('runs the first config, then a new one that comes while it waits, then exits when none comes', async () => {
    const r = await runEntrypoint(url, [{ status: 404 }, config('paddock-second'), { status: 404 }]);
    assert.equal(r.code, 0);
    assert.deepEqual(runs(r), ['run jit-first', 'run jit-paddock-second']);
    assert.ok(r.seconds >= 2, 'it did not wait for a config after the last job');
  });

  test('exits once no config comes in its wait', async () => {
    const r = await runEntrypoint(url, [{ status: 404 }], { wait: 1 });
    assert.equal(r.code, 0);
    assert.deepEqual(runs(r), ['run jit-first']);
    assert.ok(r.seconds < 6, `waited ${r.seconds}s`);
  });

  test('exits at once when its URL is refused', async () => {
    const r = await runEntrypoint(url, [{ status: 403 }], { wait: 30 });
    assert.equal(r.code, 0);
    assert.deepEqual(runs(r), ['run jit-first']);
    assert.ok(r.seconds < 10, `waited ${r.seconds}s after a refusal`);
  });

  test('treats a config with no just-in-time config as none, and runs nothing for it', async () => {
    const r = await runEntrypoint(url, [{ status: 200, body: { runner: 'paddock-second' } }], { wait: 1 });
    assert.equal(r.code, 0);
    assert.deepEqual(runs(r), ['run jit-first']);
  });

  test('keeps waiting through a network failure or a server error', async () => {
    const r = await runEntrypoint(url, [{ status: 500 }, { status: 503 }, config('paddock-second'), { status: 404 }], { wait: 6 });
    assert.deepEqual(runs(r), ['run jit-first', 'run jit-paddock-second']);
  });
});

describe('fitness: a task wipes the last job\'s checkout and temporary files and keeps the caches (pool-per-repo.md)', () => {
  const url = new ConfigURL();
  before(() => new Promise<void>((done) => url.server.listen(0, '127.0.0.1', done)));
  after(() => new Promise<void>((done) => url.server.close(() => done())));

  test('the second job finds none of the first job\'s checkout or temporary files, and finds the kept caches', async () => {
    const r = await runEntrypoint(url, [config('paddock-second'), { status: 404 }]);
    const second = r.log.slice(r.log.indexOf('run jit-paddock-second') + 1);
    const seen = second.slice(0, second.indexOf('end'));
    assert.deepEqual(seen, ['  sees _work/_tool/node/cached', '  sees .npm/cached', '  sees go/pkg/mod/cached']);
  });
});

describe('fitness: the entrypoint reads later configs only through its URL, and runs none twice (runner-isolation.md)', () => {
  const url = new ConfigURL();
  before(() => new Promise<void>((done) => url.server.listen(0, '127.0.0.1', done)));
  after(() => new Promise<void>((done) => url.server.close(() => done())));

  test('it reads only the URL from its first config, and no runner sees the URL', async () => {
    const r = await runEntrypoint(url, [config('paddock-second'), { status: 404 }]);
    assert.ok(r.log.every((l) => l !== '  sees the URL'), r.log.join('\n'));
    assert.ok(url.requests.length > 0);
    for (const req of url.requests) assert.equal(req, '/configs/5/paddock-first?X-Amz-Signature=abc');
  });

  test('a config it has run counts as none: the first config served back is never run again', async () => {
    const r = await runEntrypoint(url, [config('paddock-first')], { wait: 1 });
    assert.equal(r.code, 0);
    assert.deepEqual(runs(r), ['run jit-first']);
  });

  test('a later config served again is not run again', async () => {
    const r = await runEntrypoint(url, [config('paddock-second')], { wait: 1 });
    assert.deepEqual(runs(r), ['run jit-first', 'run jit-paddock-second']);
  });
});

describe('fitness: a task exits only with no runner registered (remove-then-terminate.md)', () => {
  const url = new ConfigURL();
  before(() => new Promise<void>((done) => url.server.listen(0, '127.0.0.1', done)));
  after(() => new Promise<void>((done) => url.server.close(() => done())));

  test('it asks for a config only once the runner has finished its job and left', async () => {
    const r = await runEntrypoint(url, [config('paddock-second'), { status: 404 }], { wait: 1 });
    const events = r.log.filter((l) => !l.startsWith('  '));
    const firstPoll = events.indexOf('poll');
    assert.deepEqual(events.slice(0, firstPoll), ['run jit-first', 'end']);
    const second = events.indexOf('run jit-paddock-second');
    assert.equal(events.slice(second).indexOf('end'), 1, events.join(' '));
  });

  test('a stop passes to the running runner, and the task exits once it has, with no wait for another config', async () => {
    const r = await runEntrypoint(url, [config('paddock-second')], { term: true });
    assert.ok(r.log.includes('term'), `the runner was not told to stop: ${r.log.join(' | ')}`);
    assert.equal(r.code, 143);
    assert.deepEqual(runs(r), ['run jit-first']);
    assert.equal(url.requests.length, 0);
  });

  test('a runner that fails ends the task, with no wait for another config', async () => {
    const r = await runEntrypoint(url, [config('paddock-second')], { exit: 3 });
    assert.equal(r.code, 3);
    assert.deepEqual(runs(r), ['run jit-first']);
    assert.equal(url.requests.length, 0);
  });
});
