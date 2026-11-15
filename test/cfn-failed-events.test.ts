/**
 * What a failed apply prints: the stack's failed events since the apply
 * started, newest first, each as its logical id, status and reason. The
 * reason sits in the stack's events, which only someone with AWS
 * credentials can read otherwise.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { REPO_ROOT } from './helpers/cfn.ts';
import { describeEvent, failedSince, readEventsSince, report, type StackEvent } from '../scripts/cfn-failed-events.mjs';

const SINCE = new Date('2026-11-14T17:16:49Z');
const at = (iso: string) => new Date(iso);

// Newest first, as DescribeStackEvents lists them: the failed apply, then
// the green apply before it, which had a failed event of its own.
const EVENTS: StackEvent[] = [
  { Timestamp: at('2026-11-14T17:18:16Z'), LogicalResourceId: 'paddock', ResourceStatus: 'UPDATE_ROLLBACK_COMPLETE' },
  { Timestamp: at('2026-11-14T17:17:44Z'), LogicalResourceId: 'paddock', ResourceStatus: 'UPDATE_ROLLBACK_IN_PROGRESS', ResourceStatusReason: 'The following resource(s) failed to update: [ListenerService].' },
  { Timestamp: at('2026-11-14T17:17:44Z'), LogicalResourceId: 'ListenerService', ResourceStatus: 'UPDATE_FAILED', ResourceStatusReason: 'Invalid request provided' },
  { Timestamp: at('2026-11-14T17:17:20Z'), LogicalResourceId: 'RunnerLogs', ResourceStatus: 'CREATE_FAILED', ResourceStatusReason: 'Resource creation cancelled' },
  { Timestamp: at('2026-11-14T17:17:00Z'), LogicalResourceId: 'paddock', ResourceStatus: 'UPDATE_IN_PROGRESS', ResourceStatusReason: 'User Initiated' },
  { Timestamp: at('2026-11-14T17:08:45Z'), LogicalResourceId: 'OldQueue', ResourceStatus: 'DELETE_FAILED', ResourceStatusReason: 'from the apply before' },
];

describe('a failed apply\'s events', () => {
  test('are the failed ones since the apply started, newest first', () => {
    assert.deepEqual(failedSince(EVENTS, SINCE).map((e) => e.LogicalResourceId), ['ListenerService', 'RunnerLogs']);
  });

  test('count an event at the very second the apply started', () => {
    const event = { Timestamp: SINCE, LogicalResourceId: 'Cluster', ResourceStatus: 'UPDATE_FAILED' };
    assert.deepEqual(failedSince([event], SINCE), [event]);
  });

  test('each print as the logical id, the status and the reason', () => {
    assert.equal(describeEvent(EVENTS[2]!), 'ListenerService UPDATE_FAILED: Invalid request provided');
    assert.equal(describeEvent({ LogicalResourceId: 'Cluster', ResourceStatus: 'UPDATE_FAILED' }), 'Cluster UPDATE_FAILED: (no reason given)');
  });
});

describe('reading a stack\'s events', () => {
  // DescribeStackEvents pages newest first, so a page that reaches back past
  // the apply's start holds every event of the apply.
  test('names the stack, and stops at the first page that reaches back past the apply\'s start', async () => {
    const pages = [
      { StackEvents: EVENTS.slice(0, 3), NextToken: 'page-2' },
      { StackEvents: EVENTS.slice(3), NextToken: 'page-3' },
      { StackEvents: [], NextToken: 'page-4' },
    ];
    const calls: Array<{ StackName?: string | undefined; NextToken?: string | undefined }> = [];
    const events = await readEventsSince(async (input) => {
      calls.push(input);
      return pages[calls.length - 1]!;
    }, 'paddock', SINCE);
    assert.deepEqual(calls, [{ StackName: 'paddock' }, { StackName: 'paddock', NextToken: 'page-2' }]);
    assert.equal(events.length, EVENTS.length);
  });

  test('stops when there is no next page', async () => {
    let calls = 0;
    const events = await readEventsSince(async () => {
      calls++;
      return { StackEvents: EVENTS.slice(0, 2) };
    }, 'paddock', SINCE);
    assert.equal(calls, 1);
    assert.equal(events.length, 2);
  });
});

describe('the report', () => {
  const run = async (read: () => Promise<StackEvent[]>) => {
    const lines: string[] = [];
    await report({ read, stack: 'paddock', since: SINCE, log: (line: string) => lines.push(line) });
    return lines;
  };

  test('prints each failed event of the apply under a line naming the stack and the start', async () => {
    assert.deepEqual(await run(async () => EVENTS), [
      'paddock\'s failed events since 2026-11-14T17:16:49.000Z, newest first:',
      '  ListenerService UPDATE_FAILED: Invalid request provided',
      '  RunnerLogs CREATE_FAILED: Resource creation cancelled',
    ]);
  });

  // An apply can fail before it changes anything, as when its change set
  // fails, and `aws cloudformation deploy` prints that reason itself.
  test('says so when the apply left no failed event', async () => {
    const lines = await run(async () => EVENTS.slice(4));
    assert.equal(lines.length, 1);
    assert.match(lines[0]!, /^paddock recorded no failed event since 2026-11-14T17:16:49\.000Z/);
  });

  // The report explains a failure the step reports anyway, so a failed read
  // must not throw past it.
  test('says so, and throws nothing, when it cannot read the events', async () => {
    const lines = await run(async () => { throw new Error('ThrottlingException'); });
    assert.equal(lines.length, 1);
    assert.match(lines[0]!, /could not read paddock's events: ThrottlingException/);
  });

  test('prints to the log alone, never to a job summary or a file', () => {
    const script = readFileSync(resolve(REPO_ROOT, 'scripts/cfn-failed-events.mjs'), 'utf-8');
    assert.doesNotMatch(script, /GITHUB_STEP_SUMMARY|node:fs|writeFile|appendFile/);
  });
});
