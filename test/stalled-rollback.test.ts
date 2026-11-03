/**
 * The guard in front of every apply, copied from gh-governor: it continues a
 * stalled update rollback, refuses a failed first create, and tells a stack
 * that does not exist from one it could not read.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { describedStatus, recoveryAction, rollbackCommands } from '../scripts/cfn-continue-stalled-rollback.mjs';

describe('the stalled-rollback guard', () => {
  test('continues only a stalled update rollback, and refuses both spellings of a failed first create', () => {
    assert.equal(recoveryAction('UPDATE_ROLLBACK_FAILED'), 'continue');
    assert.equal(recoveryAction('ROLLBACK_COMPLETE'), 'refuse');
    assert.equal(recoveryAction('ROLLBACK_FAILED'), 'refuse');
    for (const status of ['UPDATE_COMPLETE', 'UPDATE_ROLLBACK_COMPLETE', 'CREATE_COMPLETE', undefined]) {
      assert.equal(recoveryAction(status), 'proceed');
    }
  });

  test('reads a stack that does not exist as absent, and any other failed read as unreadable', () => {
    assert.deepEqual(describedStatus({ status: 0, stdout: 'UPDATE_COMPLETE', stderr: '' }), { kind: 'status', status: 'UPDATE_COMPLETE' });
    assert.deepEqual(describedStatus({ status: 254, stdout: '', stderr: 'Stack with id paddock does not exist' }), { kind: 'absent' });
    assert.deepEqual(describedStatus({ status: 254, stdout: '', stderr: 'AccessDenied' }), { kind: 'unreadable' });
  });

  test('continues the rollback, then waits for it with a waiter the CLI knows', () => {
    assert.deepEqual(rollbackCommands('paddock'), [
      ['cloudformation', 'continue-update-rollback', '--stack-name', 'paddock'],
      ['cloudformation', 'wait', 'stack-rollback-complete', '--stack-name', 'paddock'],
    ]);
  });
});
