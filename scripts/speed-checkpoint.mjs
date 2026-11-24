#!/usr/bin/env node
//
// The speed checkpoint (PRD #1, journey 3).
//
// Compares the median time from start to finish of a served repo's CI and
// delivery runs in the week after a move with the four weeks before it:
//
//   node scripts/speed-checkpoint.mjs <owner/repo> <ci-workflow> <delivery-workflow> <baseline-end> <week-start>
//
// A workflow is given by its name or its file name. The baseline is the 28
// days that end at <baseline-end>; the week is the 7 days from <week-start>.
// Both dates are ISO 8601 times. It reads the runs through `gh api`, so it
// needs a session or a login that can read the repo's Actions.
//
// The served repo is an argument, never a value in a file, and the report
// names neither the repo nor a run (docs/constraints/public-repo.md): it is
// meant to be pasted on a public issue.
//
// Only successful runs count. A failed or cancelled run stops early, so it
// would pull the median down by however many runs broke that week. A run's
// time is from its start to its last update, which for a completed run is
// when it finished. A re-run counts from its latest attempt's start.

import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { isCliEntry } from './lib/cli-entry.mjs';

/**
 * @typedef {{ id: number, created_at: string, run_started_at: string, updated_at: string }} Run
 * @typedef {{ id: number, name: string, path: string }} Workflow
 * @typedef {(path: string) => Promise<any>} Get
 * @typedef {{ median: number, runs: number }} Window
 * @typedef {{ label: string, baseline: Window, week: Window, change: number }} Compared
 * @typedef {{ baselineStart: Date, baselineEnd: Date, weekStart: Date, weekEnd: Date, workflows: Compared[], slower: boolean }} Result
 */

const DAY = 24 * 60 * 60 * 1000;
const BASELINE_DAYS = 28;
const WEEK_DAYS = 7;
/** More than this much slower misses the PRD's speed goal. */
const LIMIT = 0.1;
const PER_PAGE = 100;
/** The Actions API serves no more than 1000 results to one query. */
const MAX_PAGES = 10;

/** @param {Run} run */
export const durationSeconds = (run) => (Date.parse(run.updated_at) - Date.parse(run.run_started_at)) / 1000;

/** @param {number[]} values */
export const median = (values) => {
  if (values.length === 0) throw new Error('no runs to take a median of');
  const sorted = [...values].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 1 ? /** @type {number} */ (sorted[mid]) : (/** @type {number} */ (sorted[mid - 1]) + /** @type {number} */ (sorted[mid])) / 2;
};

/**
 * The week's median against the baseline's, as a fraction: 0.1 is 10% slower.
 * Rounded to a hundredth of a percent, so a report's figures add up.
 *
 * @param {number} before
 * @param {number} after
 */
export const change = (before, after) => Math.round(((after - before) / before) * 10000) / 10000;

/** @param {Date} d */
const iso = (d) => d.toISOString().replace(/\.000Z$/, 'Z');

/**
 * The window as GitHub `created` ranges of a day each, the last cut short.
 *
 * @param {Date} start
 * @param {Date} end
 * @returns {string[]}
 */
export const dayRanges = (start, end) => {
  const ranges = [];
  for (let from = start.getTime(); from < end.getTime(); from += DAY) {
    ranges.push(`${iso(new Date(from))}..${iso(new Date(Math.min(from + DAY, end.getTime())))}`);
  }
  return ranges;
};

/**
 * The id of the workflow with this name or file name.
 *
 * @param {Get} get
 * @param {string} repo
 * @param {string} nameOrFile
 * @returns {Promise<number>}
 */
export async function resolveWorkflow(get, repo, nameOrFile) {
  /** @type {{ workflows: Workflow[] }} */
  const { workflows } = await get(`repos/${repo}/actions/workflows?per_page=${PER_PAGE}`);
  const found = workflows.find((w) => w.name === nameOrFile || w.path.split('/').at(-1) === nameOrFile);
  if (!found) throw new Error(`the repo has no workflow named ${nameOrFile}`);
  return found.id;
}

/**
 * The workflow's successful runs created in [start, end), each once.
 *
 * @param {Get} get
 * @param {string} repo
 * @param {number} workflowId
 * @param {Date} start
 * @param {Date} end
 * @returns {Promise<Run[]>}
 */
export async function readRuns(get, repo, workflowId, start, end) {
  /** @type {Map<number, Run>} */
  const runs = new Map();
  for (const range of dayRanges(start, end)) {
    for (let page = 1; page <= MAX_PAGES; page++) {
      /** @type {{ total_count: number, workflow_runs: Run[] }} */
      const body = await get(`repos/${repo}/actions/workflows/${workflowId}/runs?status=success&per_page=${PER_PAGE}&created=${range}&page=${page}`);
      for (const run of body.workflow_runs) runs.set(run.id, run);
      if (body.workflow_runs.length < PER_PAGE || page * PER_PAGE >= body.total_count) break;
    }
  }
  // GitHub's range includes both ends; the window does not include its end.
  return [...runs.values()].filter((r) => {
    const created = Date.parse(r.created_at);
    return created >= start.getTime() && created < end.getTime();
  });
}

/**
 * @param {Run[]} runs
 * @returns {Window}
 */
const summarise = (runs) => ({ median: median(runs.map(durationSeconds)), runs: runs.length });

/**
 * @param {{ get: Get, repo: string, ci: string, delivery: string, baselineEnd: Date, weekStart: Date }} input
 * @returns {Promise<Result>}
 */
export async function checkpoint({ get, repo, ci, delivery, baselineEnd, weekStart }) {
  const baselineStart = new Date(baselineEnd.getTime() - BASELINE_DAYS * DAY);
  const weekEnd = new Date(weekStart.getTime() + WEEK_DAYS * DAY);
  /** @type {Compared[]} */
  const workflows = [];
  for (const [label, name] of /** @type {const} */ ([['CI', ci], ['Delivery', delivery]])) {
    const id = await resolveWorkflow(get, repo, name);
    const baseline = summarise(await readRuns(get, repo, id, baselineStart, baselineEnd));
    const week = summarise(await readRuns(get, repo, id, weekStart, weekEnd));
    workflows.push({ label, baseline, week, change: change(baseline.median, week.median) });
  }
  return { baselineStart, baselineEnd, weekStart, weekEnd, workflows, slower: workflows.some((w) => w.change > LIMIT) };
}

/** @param {Date} d */
const when = (d) => `${d.toISOString().slice(0, 16).replace('T', ' ')} UTC`;

/** @param {number} seconds */
const minutes = (seconds) => {
  const s = Math.round(seconds);
  return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`;
};

/** @param {number} fraction */
const percent = (fraction) => `${fraction >= 0 ? '+' : ''}${(fraction * 100).toFixed(1)}%`;

/**
 * The medians and the changes, as Markdown for the PRD. No repo, no run.
 *
 * @param {Result} result
 * @returns {string}
 */
export const formatReport = (result) => {
  const slow = result.workflows.filter((w) => w.change > LIMIT).map((w) => w.label);
  return [
    `Baseline: the four weeks from ${when(result.baselineStart)} to ${when(result.baselineEnd)}.`,
    `Week: ${when(result.weekStart)} to ${when(result.weekEnd)}.`,
    '',
    '| Workflow | Baseline median | Week median | Change |',
    '| --- | --- | --- | --- |',
    ...result.workflows.map((w) => `| ${w.label} | ${minutes(w.baseline.median)} | ${minutes(w.week.median)} | ${percent(w.change)} |`),
    '',
    slow.length === 0 ? 'Both are within 10%.' : `${slow.join(' and ')} ${slow.length === 1 ? 'is' : 'are'} more than 10% slower.`,
  ].join('\n');
};

/**
 * @param {string[]} args
 * @returns {{ repo: string, ci: string, delivery: string, baselineEnd: Date, weekStart: Date } | undefined}
 */
export const parseArgs = (args) => {
  const [repo, ci, delivery, end, start] = args;
  if (!repo || !ci || !delivery || !end || !start || !/^[^/\s]+\/[^/\s]+$/.test(repo)) return undefined;
  const baselineEnd = new Date(end);
  const weekStart = new Date(start);
  if (Number.isNaN(baselineEnd.getTime()) || Number.isNaN(weekStart.getTime())) return undefined;
  return { repo, ci, delivery, baselineEnd, weekStart };
};

/** @type {Get} */
const ghApi = async (path) => {
  const { stdout } = await promisify(execFile)('gh', ['api', path], { maxBuffer: 64 * 1024 * 1024 });
  return JSON.parse(stdout);
};

// Importable for the unit test; only the direct invocation reaches GitHub.
if (isCliEntry(import.meta.url, process.argv[1])) {
  const args = parseArgs(process.argv.slice(2));
  if (!args) {
    console.error('usage: speed-checkpoint.mjs <owner/repo> <ci-workflow> <delivery-workflow> <baseline-end> <week-start>');
    process.exit(2);
  }
  const result = await checkpoint({ get: ghApi, ...args });
  for (const w of result.workflows) console.error(`${w.label}: ${w.baseline.runs} baseline runs, ${w.week.runs} in the week`);
  console.log(formatReport(result));
}
