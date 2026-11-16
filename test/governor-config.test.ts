/**
 * Governor's config file, .github/governor.json. Governor reads it from
 * main, and does not serve a repo that has only the older
 * .github/dispatch.json. Governor's own config check holds the schema;
 * these hold what this repo relies on.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import yaml from 'js-yaml';
import { REPO_ROOT } from './helpers/cfn.ts';
import { loadWorkflow } from './helpers/workflow.ts';

const CONFIG = '.github/governor.json';

type Config = {
  $schema?: string;
  workflows?: { ci?: string };
  prReview?: { merge?: boolean; humanOnly?: Array<{ glob: string; reason: string }>; dependabot?: string[] };
  heartbeat?: { issue?: number };
  [section: string]: unknown;
};

const config = (): Config => JSON.parse(readFileSync(resolve(REPO_ROOT, CONFIG), 'utf-8')) as Config;

describe('governor\'s config', () => {
  test('is the file governor reads, and the older file is gone', () => {
    assert.ok(existsSync(resolve(REPO_ROOT, CONFIG)), `${CONFIG} is missing, so governor does not serve the repo`);
    assert.ok(!existsSync(resolve(REPO_ROOT, '.github/dispatch.json')), 'governor no longer reads .github/dispatch.json');
  });

  test('the review waits for the gate at a pull request\'s head', () => {
    const ci = config().workflows?.ci;
    assert.equal(ci, '.github/workflows/gate.yml');
    assert.ok(existsSync(resolve(REPO_ROOT, ci)));
  });

  test('a change to the config file itself waits for a person', () => {
    const globs = (config().prReview?.humanOnly ?? []).map((entry) => entry.glob);
    assert.ok(globs.includes(CONFIG), globs.join(', '));
  });

  // Dependabot keeps each image's base current, and the runner's must stay
  // within 30 days of GitHub's latest release (docs/constraints/runner-image.md).
  // A bump the review does not handle waits for a person.
  test('the review handles a Dependabot bump of every image base Dependabot watches', () => {
    const dependabot = yaml.load(readFileSync(resolve(REPO_ROOT, '.github/dependabot.yml'), 'utf-8')) as {
      updates?: Array<{ 'package-ecosystem'?: string; directory?: string }>;
    };
    const dockerfiles = (dependabot.updates ?? [])
      .filter((update) => update['package-ecosystem'] === 'docker')
      .map((update) => `${String(update.directory).replace(/^\//, '')}/Dockerfile`);
    assert.ok(dockerfiles.length >= 2, `Dependabot watches ${dockerfiles.join(', ')}`);
    const handled = config().prReview?.dependabot ?? [];
    for (const dockerfile of dockerfiles) {
      assert.ok(existsSync(resolve(REPO_ROOT, dockerfile)), `${dockerfile} is missing`);
      assert.ok(handled.includes(dockerfile), `${dockerfile} is not in ${handled.join(', ')}`);
    }
  });
});

// Governor publishes no tags, so a served repo pins its two reusable
// workflows and the config file's schema by commit. A called workflow cannot
// learn which commit it runs from, so the caller passes it again as `commit`.
// Governor's guide says to move all of them together.
describe('governor\'s checks run in this repo, at one commit of governor', () => {
  const check = loadWorkflow('.github/workflows/config-check.yml').workflow;
  const heartbeat = loadWorkflow('.github/workflows/heartbeat.yml').workflow;
  const pinOf = (uses: unknown) => String(uses).split('@')[1];

  test('the schema, both workflows and their `commit` inputs name the same commit', () => {
    const schema = /^https:\/\/raw\.githubusercontent\.com\/t-d-d\/gh-governor\/([0-9a-f]{40})\/schema\/governor\.schema\.json$/.exec(config().$schema ?? '');
    assert.ok(schema, `$schema is ${config().$schema}`);
    const pins = [
      pinOf(check.jobs?.check?.uses),
      check.jobs?.check?.with?.commit,
      pinOf(heartbeat.jobs?.check?.uses),
      heartbeat.jobs?.check?.with?.commit,
    ];
    assert.deepEqual(pins, pins.map(() => schema[1]));
  });

  test('the config check runs on a pull request that changes the config file or CLAUDE.md, and only reads', () => {
    assert.match(String(check.jobs?.check?.uses), /^t-d-d\/gh-governor\/\.github\/workflows\/config-check\.yml@/);
    assert.deepEqual(Object.keys(check.on ?? {}), ['pull_request']);
    const paths: string[] = check.on?.pull_request?.paths ?? [];
    for (const path of [CONFIG, 'CLAUDE.md']) assert.ok(paths.includes(path), `${path} is not in ${paths.join(', ')}`);
    assert.deepEqual(check.permissions, { contents: 'read' });
  });

  test('the heartbeat check runs every hour and by hand, and writes only issues', () => {
    assert.match(String(heartbeat.jobs?.check?.uses), /^t-d-d\/gh-governor\/\.github\/workflows\/heartbeat-check\.yml@/);
    assert.deepEqual(Object.keys(heartbeat.on ?? {}).sort(), ['schedule', 'workflow_dispatch']);
    assert.match(String(heartbeat.on?.schedule?.[0]?.cron), /^\d{1,2} \* \* \* \*$/);
    assert.deepEqual(heartbeat.permissions, { contents: 'read', issues: 'write' });
    assert.equal(heartbeat.jobs?.check?.with?.labels, 'heartbeat');
  });
});

describe('governor\'s newer features', () => {
  test('deploy messages, the heartbeat, waits and session rules are on', () => {
    for (const section of ['deployMessages', 'heartbeat', 'waits', 'sessionRules']) {
      const value = config()[section] as { enabled?: boolean } | undefined;
      assert.ok(value !== undefined && value.enabled !== false, `${section} is off`);
    }
    assert.ok(Number.isInteger(config().heartbeat?.issue), 'the heartbeat names its issue');
  });

  // A run of gate.yml is the review's CI, and no run on main is one: the gate
  // runs there inside Deploy and Pages. A red Deploy run is what tells a
  // merge's session, through deploy messages.
  test('CI messages stay off, since no run on main is a run of the gate\'s own file', () => {
    assert.equal(config().ciMessages, undefined);
  });

  test('CLAUDE.md carries governor\'s session rules block, whole', () => {
    const text = readFileSync(resolve(REPO_ROOT, 'CLAUDE.md'), 'utf-8');
    const block = /<!-- governor:session-rules v\d+ -->\n([\s\S]*?)\n<!-- governor:session-rules-end -->/.exec(text);
    assert.ok(block, 'no session rules block');
    for (const rule of ['1. Sign everything you write on GitHub', '2. Only an issue\'s or a PR\'s owner edits its body', '3. To change an issue or a PR you do not own, comment on it']) {
      assert.ok(block[1]!.includes(rule), rule);
    }
  });
});
