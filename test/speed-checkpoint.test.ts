/**
 * The speed checkpoint (PRD #1, journey 3): the median time from start to
 * finish of a served repo's CI and delivery runs in the week after a move,
 * against the four weeks before it on GitHub's runners. The repo and the
 * workflows come in as arguments, never from a file (public-repo.md), so the
 * runs here are fixtures and the API is a fake.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import {
  change,
  checkpoint,
  dayRanges,
  durationSeconds,
  formatReport,
  median,
  parseArgs,
  readRuns,
  resolveWorkflow,
  type Get,
  type Run,
} from '../scripts/speed-checkpoint.mjs';

const REPO = 'example-org/example-app';

/** A successful run that started at `start` and took `seconds`. */
const run = (id: number, start: string, seconds: number): Run => {
  const started = new Date(start);
  return {
    id,
    created_at: start,
    run_started_at: start,
    updated_at: new Date(started.getTime() + seconds * 1000).toISOString(),
  };
};

describe('a run\'s time', () => {
  test('runs from its start to its last update', () => {
    assert.equal(durationSeconds(run(1, '2026-11-17T10:00:00Z', 372)), 372);
  });
});

describe('the median', () => {
  test('is the middle value of an odd count', () => {
    assert.equal(median([300, 100, 200]), 200);
  });

  test('is the mean of the middle two of an even count', () => {
    assert.equal(median([400, 100, 200, 300]), 250);
  });

  test('of no runs is refused rather than reported as zero', () => {
    assert.throws(() => median([]), /no runs/);
  });
});

describe('the change', () => {
  test('is the week\'s median against the baseline\'s, as a fraction', () => {
    assert.equal(change(200, 220), 0.1);
    assert.equal(change(200, 180), -0.1);
  });
});

describe('the days queried', () => {
  // The Actions API returns at most 1000 runs for one query, so a busy
  // repo's four weeks are read a day at a time.
  test('split a window into whole days, the last one cut at the window\'s end', () => {
    assert.deepEqual(dayRanges(new Date('2026-11-16T15:32:48Z'), new Date('2026-11-18T12:00:00Z')), [
      '2026-11-16T15:32:48Z..2026-11-17T15:32:48Z',
      '2026-11-17T15:32:48Z..2026-11-18T12:00:00Z',
    ]);
  });

  test('are given in whole seconds, as GitHub reads them', () => {
    assert.deepEqual(dayRanges(new Date('2026-11-16T15:32:48.250Z'), new Date('2026-11-17T12:00:00.750Z')), [
      '2026-11-16T15:32:48Z..2026-11-17T12:00:00Z',
    ]);
  });
});

describe('finding a workflow', () => {
  const workflows = {
    total_count: 2,
    workflows: [
      { id: 11, name: 'CI', path: '.github/workflows/ci.yml' },
      { id: 22, name: 'Deliver', path: '.github/workflows/deliver.yml' },
    ],
  };
  const get: Get = async (path) => {
    assert.equal(path, `repos/${REPO}/actions/workflows?per_page=100&page=1`);
    return workflows;
  };

  test('matches its name', async () => {
    assert.equal(await resolveWorkflow(get, REPO, 'Deliver'), 22);
  });

  test('matches its file name', async () => {
    assert.equal(await resolveWorkflow(get, REPO, 'ci.yml'), 11);
  });

  test('refuses a name the repo has no workflow by', async () => {
    await assert.rejects(resolveWorkflow(get, REPO, 'Release'), /no workflow named Release/);
  });

  test('follows the pages of a repo with more than a hundred workflows', async () => {
    const many = Array.from({ length: 100 }, (_, i) => ({ id: i + 100, name: `Job ${i}`, path: `.github/workflows/job-${i}.yml` }));
    const paged: Get = async (path) =>
      path.endsWith('&page=1')
        ? { total_count: 101, workflows: many }
        : { total_count: 101, workflows: [{ id: 33, name: 'Release', path: '.github/workflows/release.yml' }] };
    assert.equal(await resolveWorkflow(paged, REPO, 'Release'), 33);
  });

  test('refuses a name two workflows share', async () => {
    const twins: Get = async () => ({
      total_count: 2,
      workflows: [
        { id: 11, name: 'CI', path: '.github/workflows/ci.yml' },
        { id: 12, name: 'CI', path: '.github/workflows/ci-nightly.yml' },
      ],
    });
    await assert.rejects(resolveWorkflow(twins, REPO, 'CI'), /more than one workflow named CI/);
  });
});

describe('reading a workflow\'s runs', () => {
  const start = new Date('2026-11-16T00:00:00Z');
  const end = new Date('2026-11-17T12:00:00Z');

  test('asks for successful runs a day at a time, and follows the pages', async () => {
    const calls: string[] = [];
    const firstPage = Array.from({ length: 100 }, (_, i) => run(i + 1, '2026-11-16T01:00:00Z', 60));
    const get: Get = async (path) => {
      calls.push(path);
      if (path.includes('created=2026-11-16T00:00:00Z..2026-11-17T00:00:00Z&page=1')) return { total_count: 101, workflow_runs: firstPage };
      if (path.includes('created=2026-11-16T00:00:00Z..2026-11-17T00:00:00Z&page=2')) return { total_count: 101, workflow_runs: [run(101, '2026-11-16T02:00:00Z', 60)] };
      return { total_count: 0, workflow_runs: [] };
    };
    const runs = await readRuns(get, REPO, 22, start, end);
    assert.equal(runs.length, 101);
    assert.deepEqual(calls, [
      `repos/${REPO}/actions/workflows/22/runs?status=success&per_page=100&created=2026-11-16T00:00:00Z..2026-11-17T00:00:00Z&page=1`,
      `repos/${REPO}/actions/workflows/22/runs?status=success&per_page=100&created=2026-11-16T00:00:00Z..2026-11-17T00:00:00Z&page=2`,
      `repos/${REPO}/actions/workflows/22/runs?status=success&per_page=100&created=2026-11-17T00:00:00Z..2026-11-17T12:00:00Z&page=1`,
    ]);
  });

  test('counts a run on a day\'s boundary once, and none created outside the window', async () => {
    // GitHub's created range includes both ends.
    const onBoundary = run(1, '2026-11-17T00:00:00Z', 60);
    const atEnd = run(2, '2026-11-17T12:00:00Z', 60);
    const get: Get = async (path) =>
      path.includes('page=1') ? { total_count: 2, workflow_runs: [onBoundary, atEnd] } : { total_count: 0, workflow_runs: [] };
    const runs = await readRuns(get, REPO, 22, start, end);
    assert.deepEqual(runs.map((r) => r.id), [1]);
  });

  test('refuses a day with more runs than the API serves, rather than drop some', async () => {
    let next = 0;
    const get: Get = async () => ({
      total_count: 1001,
      workflow_runs: Array.from({ length: 100 }, () => run(++next, '2026-11-16T01:00:00Z', 60)),
    });
    await assert.rejects(readRuns(get, REPO, 22, start, end), /more than 1000 runs/);
  });
});

describe('the checkpoint', () => {
  const baselineEnd = new Date('2026-11-15T00:00:00Z');
  const weekStart = new Date('2026-11-16T15:32:48Z');

  /** A fake API: CI's and Deliver's runs, in the baseline and in the week. */
  const fakeApi = (runs: Record<number, Run[]>): Get => async (path) => {
    if (path.endsWith('/actions/workflows?per_page=100&page=1')) {
      return { total_count: 2, workflows: [{ id: 11, name: 'CI', path: '.github/workflows/ci.yml' }, { id: 22, name: 'Deliver', path: '.github/workflows/deliver.yml' }] };
    }
    const id = Number(/workflows\/(\d+)\/runs/.exec(path)?.[1]);
    const range = /created=([^&]+)\.\.([^&]+)&page=(\d+)/.exec(path)!;
    if (range[3] !== '1') return { total_count: 0, workflow_runs: [] };
    const from = new Date(range[1]!);
    const to = new Date(range[2]!);
    const inRange = (runs[id] ?? []).filter((r) => new Date(r.created_at) >= from && new Date(r.created_at) <= to);
    return { total_count: inRange.length, workflow_runs: inRange };
  };

  const api = fakeApi({
    11: [
      run(1, '2026-10-17T23:00:00Z', 9999), // before the baseline
      run(2, '2026-10-20T10:00:00Z', 300),
      run(3, '2026-11-01T10:00:00Z', 360),
      run(4, '2026-11-10T10:00:00Z', 420),
      run(5, '2026-11-15T12:00:00Z', 9999), // between the windows
      run(6, '2026-11-17T10:00:00Z', 330),
      run(7, '2026-11-20T10:00:00Z', 390),
    ],
    22: [
      run(8, '2026-10-25T10:00:00Z', 600),
      run(9, '2026-11-05T10:00:00Z', 600),
      run(10, '2026-11-18T10:00:00Z', 720),
      run(11, '2026-11-24T10:00:00Z', 9999), // after the week
    ],
  });

  test('compares four weeks before the baseline\'s end with the week from its start', async () => {
    const result = await checkpoint({ get: api, repo: REPO, ci: 'CI', delivery: 'Deliver', baselineEnd, weekStart });
    assert.deepEqual(result.workflows.map(({ label, baseline, week }) => ({ label, baseline, week })), [
      { label: 'CI', baseline: { median: 360, runs: 3 }, week: { median: 360, runs: 2 } },
      { label: 'Delivery', baseline: { median: 600, runs: 2 }, week: { median: 720, runs: 1 } },
    ]);
    assert.equal(result.workflows[0]!.change, 0);
    assert.equal(result.workflows[1]!.change, 0.2);
    assert.equal(result.slower, true);
  });

  test('is within the limit when neither median is more than 10% slower', async () => {
    const result = await checkpoint({
      get: fakeApi({ 11: [run(1, '2026-11-01T10:00:00Z', 300), run(2, '2026-11-17T10:00:00Z', 330)], 22: [run(3, '2026-11-01T10:00:00Z', 600), run(4, '2026-11-17T10:00:00Z', 540)] }),
      repo: REPO,
      ci: 'CI',
      delivery: 'Deliver',
      baselineEnd,
      weekStart,
    });
    assert.equal(result.slower, false);
  });

  test('refuses a window with no successful runs', async () => {
    await assert.rejects(
      checkpoint({ get: fakeApi({ 11: [run(1, '2026-11-01T10:00:00Z', 300)], 22: [] }), repo: REPO, ci: 'CI', delivery: 'Deliver', baselineEnd, weekStart }),
      /no runs/,
    );
  });

  test('reports medians and changes, and never the repo or a run', async () => {
    const result = await checkpoint({ get: api, repo: REPO, ci: 'CI', delivery: 'Deliver', baselineEnd, weekStart });
    const text = formatReport(result);
    assert.equal(
      text,
      [
        'Baseline: the four weeks from 2026-10-18 00:00 UTC to 2026-11-15 00:00 UTC.',
        'Week: 2026-11-16 15:32 UTC to 2026-11-23 15:32 UTC.',
        '',
        '| Workflow | Baseline median | Week median | Change |',
        '| --- | --- | --- | --- |',
        '| CI | 6m 00s | 6m 00s | +0.00% |',
        '| Delivery | 10m 00s | 12m 00s | +20.00% |',
        '',
        'Delivery is more than 10% slower.',
      ].join('\n'),
    );
    assert.doesNotMatch(text, /example/);
  });

  test('shows a change just over the limit as over it', async () => {
    const result = await checkpoint({
      get: fakeApi({ 11: [run(1, '2026-11-01T10:00:00Z', 1000), run(2, '2026-11-17T10:00:00Z', 1001)], 22: [run(3, '2026-11-01T10:00:00Z', 10000), run(4, '2026-11-17T10:00:00Z', 11004)] }),
      repo: REPO,
      ci: 'CI',
      delivery: 'Deliver',
      baselineEnd,
      weekStart,
    });
    const text = formatReport(result);
    assert.match(text, /\| Delivery \| 166m 40s \| 183m 24s \| \+10\.04% \|/);
    assert.match(text, /Delivery is more than 10% slower\./);
  });
});

describe('the arguments', () => {
  test('are the repo, the two workflows and the two dates', () => {
    assert.deepEqual(parseArgs([REPO, 'CI', 'Deliver', '2026-11-15T00:00:00Z', '2026-11-16T15:32:48Z']), {
      repo: REPO,
      ci: 'CI',
      delivery: 'Deliver',
      baselineEnd: new Date('2026-11-15T00:00:00Z'),
      weekStart: new Date('2026-11-16T15:32:48Z'),
    });
  });

  test('refuse a repo without its owner, or a date that does not parse', () => {
    assert.equal(parseArgs(['example-app', 'CI', 'Deliver', '2026-11-15T00:00:00Z', '2026-11-16T15:32:48Z']), undefined);
    assert.equal(parseArgs([REPO, 'CI', 'Deliver', 'mid-November', '2026-11-16T15:32:48Z']), undefined);
    assert.equal(parseArgs([REPO, 'CI']), undefined);
  });

  test('refuse a date without its time zone, which would be read as local time', () => {
    assert.equal(parseArgs([REPO, 'CI', 'Deliver', '2026-11-15T00:00:00', '2026-11-16T15:32:48Z']), undefined);
    assert.equal(parseArgs([REPO, 'CI', 'Deliver', '2026-11-15T00:00:00Z', '2026-11-16']), undefined);
    assert.equal(parseArgs([REPO, 'CI', 'Deliver', '2026-11-15T01:00:00+01:00', '2026-11-16T15:32:48Z'])?.baselineEnd.toISOString(), '2026-11-15T00:00:00.000Z');
  });

  test('refuse a week that starts before the baseline ends', () => {
    assert.equal(parseArgs([REPO, 'CI', 'Deliver', '2026-11-17T00:00:00Z', '2026-11-16T15:32:48Z']), undefined);
  });

  test('refuse a week that has not ended, which would be reported as a whole one', () => {
    const now = new Date('2026-11-20T00:00:00Z');
    assert.equal(parseArgs([REPO, 'CI', 'Deliver', '2026-11-15T00:00:00Z', '2026-11-16T15:32:48Z'], now), undefined);
    assert.ok(parseArgs([REPO, 'CI', 'Deliver', '2026-11-08T00:00:00Z', '2026-11-12T00:00:00Z'], now));
  });
});
