#!/usr/bin/env node
//
// Print why an apply failed.
//
// When `aws cloudformation deploy` fails, it says only that the stack did
// not update and to run `describe-stack-events`. The reason sits in the
// stack's events, which only someone with AWS credentials can read. The
// deploy runs this after a failed apply, so the reason reaches its log:
//
//   node scripts/cfn-failed-events.mjs <stack-name> <since>
//
// <since> is when the apply started, as an ISO 8601 time. It prints each
// failed event since then, newest first, as its logical id, status and
// reason, and exits 0: the step that calls it fails on the apply's own exit
// code. A reason can name a resource's ARN, which carries the account id.
// It prints to the log alone, where the credentials step has masked the id,
// never to a job summary (docs/constraints/public-repo.md).

import { CloudFormationClient, DescribeStackEventsCommand } from '@aws-sdk/client-cloudformation';
import { isCliEntry } from './lib/cli-entry.mjs';

/**
 * @typedef {{
 *   Timestamp?: Date | undefined,
 *   LogicalResourceId?: string | undefined,
 *   ResourceStatus?: string | undefined,
 *   ResourceStatusReason?: string | undefined,
 * }} StackEvent
 * @typedef {{ StackEvents?: StackEvent[] | undefined, NextToken?: string | undefined }} EventsPage
 * @typedef {(input: { StackName: string, NextToken?: string }) => Promise<EventsPage>} DescribeEvents
 */

/** A bound on the pages read, so a stack with a long history cannot hold the step. */
const MAX_PAGES = 20;

/**
 * The failed events at or after `since`, in the order given: newest first,
 * as DescribeStackEvents lists them.
 *
 * @param {StackEvent[]} events
 * @param {Date} since
 * @returns {StackEvent[]}
 */
export const failedSince = (events, since) =>
  events.filter((e) => e.Timestamp !== undefined && e.Timestamp >= since && (e.ResourceStatus ?? '').endsWith('_FAILED'));

/**
 * One event as a line of the log.
 *
 * @param {StackEvent} event
 * @returns {string}
 */
export const describeEvent = (event) =>
  `${event.LogicalResourceId} ${event.ResourceStatus}: ${event.ResourceStatusReason ?? '(no reason given)'}`;

/**
 * The stack's events, newest first, back to the first page that reaches past
 * `since`. Every event of the apply is on that page or a newer one.
 *
 * @param {DescribeEvents} describe
 * @param {string} stack
 * @param {Date} since
 * @returns {Promise<StackEvent[]>}
 */
export async function readEventsSince(describe, stack, since) {
  /** @type {StackEvent[]} */
  const events = [];
  /** @type {string | undefined} */
  let token;
  for (let page = 0; page < MAX_PAGES; page++) {
    const { StackEvents = [], NextToken } = await describe(token === undefined ? { StackName: stack } : { StackName: stack, NextToken: token });
    events.push(...StackEvents);
    const oldest = StackEvents.at(-1)?.Timestamp;
    if (!NextToken || (oldest !== undefined && oldest < since)) break;
    token = NextToken;
  }
  return events;
}

/**
 * Print the apply's failed events. A failed read is printed too, and never
 * thrown: the step fails on the apply's code either way.
 *
 * @param {{ read: () => Promise<StackEvent[]>, stack: string, since: Date, log?: (line: string) => void }} io
 * @returns {Promise<void>}
 */
export async function report({ read, stack, since, log = console.log }) {
  /** @type {StackEvent[]} */
  let failed;
  try {
    failed = failedSince(await read(), since);
  } catch (err) {
    log(`could not read ${stack}'s events: ${err instanceof Error ? err.message : String(err)}`);
    return;
  }
  if (failed.length === 0) {
    log(`${stack} recorded no failed event since ${since.toISOString()}. The error above is the apply's own.`);
    return;
  }
  log(`${stack}'s failed events since ${since.toISOString()}, newest first:`);
  for (const event of failed) log(`  ${describeEvent(event)}`);
}

/**
 * @param {string | undefined} stack
 * @param {string | undefined} sinceArg
 */
async function main(stack, sinceArg) {
  const since = new Date(sinceArg ?? '');
  if (!stack || Number.isNaN(since.getTime())) {
    console.error('usage: cfn-failed-events.mjs <stack-name> <since, ISO 8601>');
    process.exit(2);
  }
  // The client is made inside the read, so report catches a failure to make
  // it as it catches a failed read.
  await report({
    read: () => {
      const cfn = new CloudFormationClient({});
      return readEventsSince((input) => cfn.send(new DescribeStackEventsCommand(input)), stack, since);
    },
    stack,
    since,
  });
}

// Importable for the unit test; only the direct invocation touches AWS.
if (isCliEntry(import.meta.url, process.argv[1])) {
  await main(process.argv[2], process.argv[3]);
}
