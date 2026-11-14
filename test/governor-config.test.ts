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
import { REPO_ROOT } from './helpers/cfn.ts';

const CONFIG = '.github/governor.json';

type Config = {
  workflows?: { ci?: string };
  prReview?: { merge?: boolean; humanOnly?: Array<{ glob: string; reason: string }> };
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
});
