#!/usr/bin/env node
//
// The speed checkpoint (PRD #1, journey 3).
//
// Compares the median time from start to finish of a served repo's CI and
// delivery runs in the week after a move with the four weeks before it:
//
//   node scripts/speed-checkpoint.mjs <owner/repo> <ci-workflow> <delivery-workflow> <baseline-end> <week-start> [<changes-job>]
//
// A workflow is given by its name or its file name. The baseline is the 28
// days that end at <baseline-end>; the week is the 7 days from <week-start>.
// Both dates are ISO 8601 times with a time zone. The week must start after
// the baseline ends, and be over before the script runs. It reads the runs
// through `gh api`, so it needs a login that can read the repo's Actions.
//
// The served repo is an argument, never a value in a file, and the report
// names neither the repo nor a run (docs/constraints/public-repo.md): it is
// meant to be pasted on a public issue.
//
// Only successful runs count. A failed or cancelled run stops early, so it
// would pull the median down by however many runs broke that week. A run's
// time is from its start to its last update, which for a completed run is
// when it finished. A re-run counts from its latest attempt's start.
//
// <changes-job> names CI's change-detection job. When it is given, a run in
// which no other job ran is left out: it finishes in seconds whatever the
// runners, so how many of them a window holds would move the median more
// than the runners do. The report says how many were left out.

import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { isCliEntry } from './lib/cli-entry.mjs';

/**
 * @typedef {{ id: number, created_at: string, run_started_at: string, updated_at: string }} Run
 * @typedef {{ id: number, name: string, path: string }} Workflow
 * @typedef {{ name: string, conclusion: string | null }} Job
 * @typedef {(path: string) => Promise<any>} Get
 * @typedef {{ median: number, runs: number, left?: number }} Window
 * @typedef {{ label: string, baseline: Window, week: Window, change: number }} Compared
 * @typedef {{ baselineStart: Date, baselineEnd: Date, weekStart: Date, weekEnd: Date, workflows: Compared[], slower: boolean, filtered: boolean }} Result
 */

const DAY = 24 * 60 * 60 * 1000;
const BASELINE_DAYS = 28;
const WEEK_DAYS = 7;
/** More than this much slower misses the PRD's speed goal. */
const LIMIT = 0.1;
const PER_PAGE = 100;
/** The Actions API serves no more than 1000 results to one query. */
const MAX_PAGES = 10;
/** A bound on the pages of workflows read. */
const MAX_WORKFLOW_PAGES = 10;

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

/**
 * A time in whole seconds. GitHub's `created` filter takes no fraction, and
 * a run's `created_at` has none, so flooring loses no run.
 *
 * @param {Date} d
 */
const iso = (d) => `${d.toISOString().slice(0, 19)}Z`;

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
  /** @type {Workflow[]} */
  const workflows = [];
  for (let page = 1; page <= MAX_WORKFLOW_PAGES; page++) {
    /** @type {{ total_count: number, workflows: Workflow[] }} */
    const body = await get(`repos/${repo}/actions/workflows?per_page=${PER_PAGE}&page=${page}`);
    workflows.push(...body.workflows);
    if (body.workflows.length < PER_PAGE || page * PER_PAGE >= body.total_count) break;
  }
  const found = workflows.filter((w) => w.name === nameOrFile || w.path.split('/').at(-1) === nameOrFile);
  if (found.length === 0) throw new Error(`the repo has no workflow named ${nameOrFile}`);
  // Two workflows can share a name; picking one would time the wrong runs.
  if (found.length > 1) throw new Error(`the repo has more than one workflow named ${nameOrFile}; give its file name`);
  return /** @type {Workflow} */ (found[0]).id;
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
      // The API stops at the last page it serves; the runs past it would be
      // missing from the median without a word.
      if (page === MAX_PAGES) throw new Error(`a day has more than ${MAX_PAGES * PER_PAGE} runs, more than the API serves to one query`);
    }
  }
  // GitHub's range includes both ends; the window does not include its end.
  return [...runs.values()].filter((r) => {
    const created = Date.parse(r.created_at);
    return created >= start.getTime() && created < end.getTime();
  });
}

/**
 * Whether the named job was the only one of a run's jobs that ran. A job
 * that did not run is listed as skipped.
 *
 * @param {Job[]} jobs
 * @param {string} name
 */
export const ranOnly = (jobs, name) => {
  const ran = jobs.filter((j) => j.conclusion !== 'skipped');
  return ran.length > 0 && ran.every((j) => j.name === name);
};

/**
 * The runs, less those in which only the named job ran. Reads each run's
 * jobs, of its latest attempt, as the run's time is.
 *
 * @param {Get} get
 * @param {string} repo
 * @param {Run[]} runs
 * @param {string} name
 * @returns {Promise<{ kept: Run[], left: number }>}
 */
async function dropRanOnly(get, repo, runs, name) {
  /** @type {Run[]} */
  const kept = [];
  for (const run of runs) {
    /** @type {{ jobs: Job[] }} */
    const { jobs } = await get(`repos/${repo}/actions/runs/${run.id}/jobs?per_page=${PER_PAGE}`);
    if (!ranOnly(jobs, name)) kept.push(run);
  }
  return { kept, left: runs.length - kept.length };
}

/**
 * @param {Run[]} runs
 * @returns {Window}
 */
const summarise = (runs) => ({ median: median(runs.map(durationSeconds)), runs: runs.length });

/**
 * One window's runs, summarised, with those that ran only the change-detection
 * job left out when it is named.
 *
 * @param {Get} get
 * @param {string} repo
 * @param {Run[]} runs
 * @param {string | undefined} changesJob
 * @returns {Promise<Window>}
 */
async function windowOf(get, repo, runs, changesJob) {
  if (changesJob === undefined) return summarise(runs);
  const { kept, left } = await dropRanOnly(get, repo, runs, changesJob);
  return { ...summarise(kept), left };
}

/**
 * @param {{ get: Get, repo: string, ci: string, delivery: string, baselineEnd: Date, weekStart: Date, changesJob?: string }} input
 * @returns {Promise<Result>}
 */
export async function checkpoint({ get, repo, ci, delivery, baselineEnd, weekStart, changesJob }) {
  const baselineStart = new Date(baselineEnd.getTime() - BASELINE_DAYS * DAY);
  const weekEnd = new Date(weekStart.getTime() + WEEK_DAYS * DAY);
  /** @type {Compared[]} */
  const workflows = [];
  for (const [label, name] of /** @type {const} */ ([['CI', ci], ['Delivery', delivery]])) {
    const id = await resolveWorkflow(get, repo, name);
    const baseline = await windowOf(get, repo, await readRuns(get, repo, id, baselineStart, baselineEnd), changesJob);
    const week = await windowOf(get, repo, await readRuns(get, repo, id, weekStart, weekEnd), changesJob);
    workflows.push({ label, baseline, week, change: change(baseline.median, week.median) });
  }
  return { baselineStart, baselineEnd, weekStart, weekEnd, workflows, slower: workflows.some((w) => w.change > LIMIT), filtered: changesJob !== undefined };
}

/** @param {Date} d */
const when = (d) => `${d.toISOString().slice(0, 16).replace('T', ' ')} UTC`;

/** @param {number} seconds */
const minutes = (seconds) => {
  const s = Math.round(seconds);
  return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`;
};

/** @param {number} fraction */
// Two places, as `change` keeps: a change just over the limit never shows as 10.0%.
const percent = (fraction) => `${fraction >= 0 ? '+' : ''}${(fraction * 100).toFixed(2)}%`;

/**
 * The medians and the changes, as Markdown for the PRD. No repo, no run.
 *
 * @param {Result} result
 * @returns {string}
 */
export const formatReport = (result) => {
  const slow = result.workflows.filter((w) => w.change > LIMIT).map((w) => w.label);
  const [first, second] = result.workflows;
  const left = result.filtered && first && second
    ? ['', `Runs that ran only the change-detection job are left out: ${first.baseline.left ?? 0} in the baseline and ${first.week.left ?? 0} in the week for ${first.label}, ${second.baseline.left ?? 0} and ${second.week.left ?? 0} for ${second.label}.`]
    : [];
  return [
    `Baseline: the four weeks from ${when(result.baselineStart)} to ${when(result.baselineEnd)}.`,
    `Week: ${when(result.weekStart)} to ${when(result.weekEnd)}.`,
    '',
    '| Workflow | Baseline median | Week median | Change |',
    '| --- | --- | --- | --- |',
    ...result.workflows.map((w) => `| ${w.label} | ${minutes(w.baseline.median)} | ${minutes(w.week.median)} | ${percent(w.change)} |`),
    '',
    slow.length === 0 ? 'Both are within 10%.' : `${slow.join(' and ')} ${slow.length === 1 ? 'is' : 'are'} more than 10% slower.`,
    ...left,
  ].join('\n');
};

/** An ISO 8601 time that names its zone, so it is never read as local time. */
const ZONED_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:\d{2})$/;

/**
 * @param {string[]} args
 * @param {Date} [now]
 * @returns {{ repo: string, ci: string, delivery: string, baselineEnd: Date, weekStart: Date, changesJob?: string } | undefined}
 */
export const parseArgs = (args, now = new Date()) => {
  const [repo, ci, delivery, end, start, changesJob] = args;
  if (!repo || !ci || !delivery || !end || !start || !/^[^/\s]+\/[^/\s]+$/.test(repo)) return undefined;
  if (!ZONED_TIME.test(end) || !ZONED_TIME.test(start)) return undefined;
  const baselineEnd = new Date(end);
  const weekStart = new Date(start);
  if (Number.isNaN(baselineEnd.getTime()) || Number.isNaN(weekStart.getTime())) return undefined;
  // Overlapping windows would count the same runs on both sides.
  if (weekStart.getTime() < baselineEnd.getTime()) return undefined;
  // A week still under way would be reported as a whole one.
  if (weekStart.getTime() + WEEK_DAYS * DAY > now.getTime()) return undefined;
  return changesJob ? { repo, ci, delivery, baselineEnd, weekStart, changesJob } : { repo, ci, delivery, baselineEnd, weekStart };
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
    console.error('usage: speed-checkpoint.mjs <owner/repo> <ci-workflow> <delivery-workflow> <baseline-end> <week-start> [<changes-job>]');
    console.error('The dates are ISO 8601 times with a zone. The week starts after the baseline ends and must be over.');
    process.exit(2);
  }
  const result = await checkpoint({ get: ghApi, ...args });
  for (const w of result.workflows) console.error(`${w.label}: ${w.baseline.runs} baseline runs, ${w.week.runs} in the week`);
  console.log(formatReport(result));
}
