/**
 * The README's operator guide, which takes a stranger from a fork to a first
 * job on their own runners. These checks hold the guide to the stacks and
 * the workflows, so an alarm, a parameter or a secret added later cannot go
 * missing from it. Whether the steps work end to end is a check by hand in a
 * scratch account.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { loadTemplate, resourcesOfType, REPO_ROOT } from './helpers/cfn.ts';
import { loadWorkflow } from './helpers/workflow.ts';

const readme = readFileSync(resolve(REPO_ROOT, 'README.md'), 'utf8');
const guideStart = readme.indexOf('## Deploying your own');
const guide = guideStart < 0 ? '' : readme.slice(guideStart);

describe('the README\'s operator guide', () => {
  test('exists', () => {
    assert.ok(guideStart >= 0, 'the README has a "Deploying your own" section');
  });

  test('names and explains every alarm the main stack defines', () => {
    const tpl = loadTemplate('infra/paddock.cfn.yml');
    const names = resourcesOfType(tpl, 'AWS::CloudWatch::Alarm').map(([, r]) => String(r.Properties?.AlarmName));
    assert.ok(names.length > 0);
    for (const name of names) assert.ok(guide.includes(`\`${name}\``), `the guide names the alarm ${name}`);
  });

  test('names every parameter the operator writes, and every secret the deploy reads', () => {
    const tpl = loadTemplate('infra/paddock.cfn.yml');
    const raw = readFileSync(resolve(REPO_ROOT, 'infra/paddock.cfn.yml'), 'utf8');
    // The stack reads these at deploy, or grants the listener a read of one
    // named parameter. The listener writes /paddock/runners/* itself, so a
    // wildcard path is left out.
    const parameters = new Set([
      ...Object.values(tpl.Parameters ?? {})
        .filter((p) => p.Type?.startsWith('AWS::SSM::Parameter::Value') && typeof p.Default === 'string')
        .map((p) => p.Default as string),
      ...[...raw.matchAll(/:parameter(\/paddock\/[a-z0-9-]+)(?![a-z0-9/-])/g)].map((m) => m[1]!),
    ]);
    const { codeOnly } = loadWorkflow('.github/workflows/deploy.yml');
    const secrets = new Set([...codeOnly.matchAll(/secrets\.([A-Z0-9_]+)/g)].map((m) => m[1]!));
    assert.ok(parameters.size > 0 && secrets.size > 0);
    for (const value of [...parameters, ...secrets]) {
      assert.ok(guide.includes(`\`${value}\``), `the guide names ${value}`);
    }
  });

  test('names the bootstrap stack\'s parameters', () => {
    const tpl = loadTemplate('infra/bootstrap.cfn.yml');
    for (const name of Object.keys(tpl.Parameters ?? {})) {
      assert.ok(guide.includes(`\`${name}\``), `the guide names the bootstrap parameter ${name}`);
    }
  });

  test('names the cost tags', () => {
    for (const tag of ['paddock:repo', 'paddock:pool']) assert.ok(guide.includes(`\`${tag}\``), `the guide names ${tag}`);
  });

  test('takes the steps in order: the App before the first deploy, the smoke job before a real repo', () => {
    const at = (text: string) => {
      const i = guide.indexOf(text);
      assert.ok(i >= 0, `the guide has "${text}"`);
      return i;
    };
    const order = [
      '### Create the GitHub App',
      '### Apply the bootstrap stack',
      '### Deploy',
      '### Run a first job on a scratch repo',
      '### Serve a real repo',
    ].map(at);
    assert.deepEqual(order, [...order].sort((a, b) => a - b));
  });
});
