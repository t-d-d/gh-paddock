/**
 * The runner lane page (docs/pages/) and the workflow that publishes it on
 * GitHub Pages. The page shows the v1 design to a reader who has not read
 * the code. It publishes from main only, and only after the same gate a
 * deploy waits on, so a page that breaks the scan never goes out.
 *
 * Whether the page reads well in light and dark, at a phone's width and a
 * desktop's, needs a browser; these tests hold what a browser would need
 * from the file: a viewport, a dark palette, and nothing loaded from
 * elsewhere.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { REPO_ROOT } from './helpers/cfn.ts';
import { loadWorkflow, stepById, stepsOf } from './helpers/workflow.ts';

const { workflow } = loadWorkflow('.github/workflows/pages.yml');
const PAGES_DIR = 'docs/pages';
const PAGE = 'runner-lane.html';
const PAGE_URL = `https://t-d-d.github.io/gh-paddock/${PAGE}`;

const read = (path: string) => readFileSync(resolve(REPO_ROOT, path), 'utf-8');
const triggers = (w: Record<string, any>): string[] =>
  typeof w.on === 'string' ? [w.on] : Array.isArray(w.on) ? w.on : Object.keys(w.on ?? {});

describe('the Pages workflow', () => {
  test('runs on a push to main and by hand, never on a pull request', () => {
    assert.deepEqual(triggers(workflow).sort(), ['push', 'workflow_dispatch']);
    assert.deepEqual(workflow.on.push, { branches: ['main'] });
  });

  test('publishes only after the gate passes', () => {
    assert.equal(workflow.jobs?.gate?.uses, './.github/workflows/gate.yml');
    const publish = workflow.jobs?.publish;
    assert.ok(publish, 'a publish job');
    assert.deepEqual([publish.needs].flat(), ['gate']);
    assert.equal(publish.if, undefined, 'no condition that could run it past a red gate');
  });

  test('publishes from main alone: a run by hand on another branch publishes nothing', () => {
    assert.equal(workflow.jobs?.publish?.environment?.name, 'github-pages');
    const steps = stepsOf(workflow, 'publish');
    const guard = stepById(steps, 'main');
    assert.equal(steps.indexOf(guard), 0, 'the guard runs first');
    // Run the guard as Actions runs it, on main and off it.
    const exits = (ref: string) =>
      spawnSync('bash', ['-eo', 'pipefail', '-c', guard.run ?? ''], {
        env: { PATH: process.env.PATH, GITHUB_REF: ref },
        stdio: 'pipe',
      }).status;
    assert.equal(exits('refs/heads/main'), 0, 'a run on main goes on');
    for (const ref of ['refs/heads/feature', 'refs/heads/main-old', 'refs/tags/main', 'refs/pull/1/merge']) {
      assert.notEqual(exits(ref), 0, `a run on ${ref} stops`);
    }
  });

  test('uploads docs/pages and nothing else, and deploys it', () => {
    const steps = stepsOf(workflow, 'publish');
    const upload = steps.find((s) => s.uses?.startsWith('actions/upload-pages-artifact@'));
    assert.equal(upload?.with?.path, PAGES_DIR);
    assert.ok(steps.some((s) => s.uses?.startsWith('actions/deploy-pages@')));
  });

  test('holds read access, and Pages and OIDC write in the publish job alone', () => {
    assert.deepEqual(workflow.permissions, { contents: 'read' });
    assert.deepEqual(workflow.jobs?.publish?.permissions, { contents: 'read', pages: 'write', 'id-token': 'write' });
  });

  test('never cancels a publish in progress', () => {
    assert.equal(workflow.concurrency?.['cancel-in-progress'], false);
  });

  // GitHub keeps one pending run per group and cancels the older. A run by
  // hand on another branch, sharing main's group, would cancel main's
  // pending publish and then stop at the guard, leaving main's page unsent.
  test('queues each ref apart, so a run off main never takes a pending main run\'s place', () => {
    assert.match(String(workflow.concurrency?.group), /\$\{\{\s*github\.ref\s*\}\}/);
  });
});

describe('fitness: the Pages workflow runs on GitHub\'s runners, never on the runners this repo deploys (public-repo.md)', () => {
  test('every job that runs steps runs on a GitHub-hosted ubuntu runner', () => {
    for (const [id, job] of Object.entries(workflow.jobs ?? {}) as [string, { uses?: string; 'runs-on'?: string }][]) {
      if (job.uses) continue;
      assert.match(String(job['runs-on']), /^ubuntu-/, `${id} runs on ${job['runs-on']}`);
    }
  });
});

describe('the runner lane page', () => {
  const html = read(`${PAGES_DIR}/${PAGE}`);

  test('fits a phone: it sets the viewport to the device width', () => {
    assert.match(html, /<meta name="viewport" content="width=device-width, initial-scale=1">/);
  });

  test('has a dark palette as well as a light one', () => {
    assert.match(html, /@media \(prefers-color-scheme: dark\)/);
    assert.match(html, /color-scheme: light dark/);
  });

  test('loads nothing from elsewhere: no script, style, font or image off the page', () => {
    assert.doesNotMatch(html, /<(?:script|img|link|iframe)[^>]+(?:src|href)="(?:https?:)?\/\//i);
    assert.doesNotMatch(html, /@import|url\((?:'|")?(?:https?:)?\/\//i);
  });

  test('shows every part of the v1 design the issue lists', () => {
    for (const part of [
      'scale set', 'single-use', 'warm', 'remove', 'busy', 'interrupt', 'failed start',
      'Spot', 'on-demand', 'later',
    ]) assert.ok(html.toLowerCase().includes(part.toLowerCase()), `the page mentions ${part}`);
  });

  test('the site root leads to the page', () => {
    assert.match(read(`${PAGES_DIR}/index.html`), new RegExp(`href="${PAGE}"`));
  });
});

describe('the README', () => {
  test('links the runner lane page', () => {
    assert.ok(read('README.md').includes(PAGE_URL));
  });
});
