/**
 * The public-repo scan's rules, one finding kind at a time
 * (docs/constraints/public-repo.md). The fitness suite runs the scan over
 * every tracked file; this suite pins what each rule refuses and lets pass.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { scanText } from '../scripts/public-repo-scan.mjs';

const kinds = (path: string, text: string) => scanText(path, text).map((f) => f.kind);
// Built from parts, as in the scan itself: no file may spell the name whole.
const served = ['pod', 'stow'].join('');
// So are the ids each rule refuses, or this file would fail the scan.
const account = ['210987', '654321'].join('');
const installation = ['8123', '4567'].join('');
const sessionId = ['session_01', 'ABCDEFGHIJKLMNOPQRSTUV'].join('');
const issue = ['44', '69'].join('');
const pull = ['12', '34'].join('');

describe('the public-repo scan', () => {
  test('refuses a twelve-digit account id, AWS\'s documented example aside', () => {
    assert.deepEqual(kinds('infra/x.yml', `arn:aws:iam::${account}:role/x`), ['account']);
    assert.deepEqual(kinds('test/x.ts', 'arn:aws:iam::123456789012:role/x'), []);
    assert.deepEqual(kinds('test/x.ts', 'a 13-digit 1234567890123 is no account'), []);
  });

  test('refuses an ARN\'s account id even inside a longer identifier', () => {
    assert.deepEqual(kinds('scripts/x.mjs', `arn:aws:ecs:eu-west-2:${account}:task/paddock/abc`), ['account']);
  });

  test('refuses a GitHub App installation id', () => {
    assert.deepEqual(kinds('docs/x.md', `installation ${installation}`), ['installation']);
    assert.deepEqual(kinds('docs/x.md', 'installation 42'), []);
  });

  test('refuses a claude.ai environment or session id, but not a short fake', () => {
    assert.deepEqual(kinds('README.md', sessionId), ['claude-id']);
    assert.deepEqual(kinds('README.md', sessionId.replace('session_', 'env_')), ['claude-id']);
    assert.deepEqual(kinds('README.md', sessionId.replace('session_', 'cse_')), ['claude-id']);
    assert.deepEqual(kinds('test/x.ts', 'session_01OWNER'), []);
  });

  test('refuses an issue reference of four or more digits, and passes a hex colour', () => {
    assert.deepEqual(kinds('docs/x.md', `see #${issue}`), ['issue-number']);
    assert.deepEqual(kinds('docs/x.md', `see https://github.com/o/r/pull/${pull}`), ['issue-number']);
    assert.deepEqual(kinds('docs/x.md', 'see #12'), []);
    assert.deepEqual(kinds('docs/x.md', 'color: #1f2937ff'), []);
  });

  test('a doc may name the served service bare; no file names it otherwise, or names its repo', () => {
    assert.deepEqual(kinds('README.md', `first built for ${served}, <https://${served}.com>`), []);
    assert.deepEqual(kinds('docs/pools.md', `the ${served}-test scale set`), ['name']);
    assert.deepEqual(kinds('scripts/x.mjs', `built for ${served}`), ['name']);
    assert.deepEqual(kinds('README.md', `see owner/${served}`), ['repo']);
    assert.deepEqual(kinds('README.md', `https://github.com/Owner/${served.toUpperCase()}`), ['repo']);
  });

  test('reads a file\'s path as well as its text', () => {
    assert.deepEqual(kinds(`test/${served}.test.ts`, ''), ['name']);
  });
});
