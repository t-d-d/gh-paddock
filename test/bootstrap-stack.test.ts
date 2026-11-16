/**
 * The bootstrap stack: the role a run on main assumes to deploy
 * (docs/constraints/deploy.md), and the registry's scan-on-push rule. The
 * App key is no part of a stack (docs/constraints/public-repo.md).
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { resolve } from 'node:path';
import { loadTemplate, onlyResource, resourcesOfType, REPO_ROOT } from './helpers/cfn.ts';

const tpl = loadTemplate('infra/bootstrap.cfn.yml');

describe('the bootstrap stack', () => {
  test('the deploy role trusts only a run on main of this repo, named by its ids as well as its names', () => {
    const trust = tpl.Resources.DeployRole?.Properties?.AssumeRolePolicyDocument?.Statement;
    assert.equal(trust?.length, 1);
    const [statement] = trust;
    assert.equal(statement.Action, 'sts:AssumeRoleWithWebIdentity');
    assert.deepEqual(statement.Condition?.StringEquals, {
      'token.actions.githubusercontent.com:aud': 'sts.amazonaws.com',
      'token.actions.githubusercontent.com:sub':
        'repo:${GitHubOwner}@${GitHubOwnerId}/${GitHubRepo}@${GitHubRepoId}:ref:refs/heads/main',
    });
  });

  test('the repo\'s ids are numbers, so a name cannot stand in for one', () => {
    assert.equal(tpl.Parameters?.GitHubOwnerId?.AllowedPattern, '^[1-9][0-9]*$');
    assert.equal(tpl.Parameters?.GitHubRepoId?.AllowedPattern, '^[1-9][0-9]*$');
  });

  test('hands the deploy role\'s ARN out, for the AWS_DEPLOY_ROLE_ARN secret', () => {
    assert.equal(tpl.Outputs?.DeployRoleArn?.Value, '${DeployRole.Arn}');
  });
});

// The operator writes the key by hand to the SecureString parameter
// /paddock/github-app. CloudFormation writes only a String parameter, which
// would store the key unencrypted and put its value in a public template.
describe('fitness: no stack creates the App key or a Secrets Manager secret (public-repo.md)', () => {
  const files = readdirSync(resolve(REPO_ROOT, 'infra'))
    .filter((name) => name.endsWith('.cfn.yml'))
    .map((name) => `infra/${name}`);

  test('every stack in infra/ is read', () => {
    assert.ok(files.includes('infra/bootstrap.cfn.yml') && files.includes('infra/paddock.cfn.yml'), files.join(', '));
  });

  for (const file of files) {
    const stack = loadTemplate(file);

    test(`${file} defines no Secrets Manager secret`, () => {
      const secrets = Object.entries(stack.Resources).filter(([, r]) => String(r.Type).startsWith('AWS::SecretsManager::'));
      assert.deepEqual(secrets.map(([name]) => name), []);
    });

    // By its logical id, name or description, since a stack could give the
    // parameter any name.
    test(`${file} defines no parameter for the App key`, () => {
      const parameters = resourcesOfType(stack, 'AWS::SSM::Parameter').filter((entry) =>
        /github.?app|app.?key/i.test(JSON.stringify(entry)),
      );
      assert.deepEqual(parameters.map(([name]) => name), []);
    });
  }

  // A comment that sends the operator to a secret the stack no longer has
  // would have them look for the key in the wrong place.
  test('no stack or workflow says the App key is in a secret', () => {
    const workflows = readdirSync(resolve(REPO_ROOT, '.github/workflows')).map((name) => `.github/workflows/${name}`);
    for (const file of [...files, ...workflows]) {
      const text = readFileSync(resolve(REPO_ROOT, file), 'utf-8').replace(/\s*\n\s*#?\s*/g, ' ');
      assert.doesNotMatch(text, /Secrets Manager|bootstrap stack's secret/i, file);
    }
  });
});

// Why the rule lives in the registry and not on each repository is in the
// stack's comment.
describe('the registry\'s scanning configuration', () => {
  test('the bootstrap stack sets basic scanning, with one scan-on-push rule for the paddock- repositories', () => {
    const config = onlyResource(tpl, 'AWS::ECR::RegistryScanningConfiguration');
    assert.equal(config.Properties?.ScanType, 'BASIC');
    assert.deepEqual(config.Properties?.Rules, [
      { ScanFrequency: 'SCAN_ON_PUSH', RepositoryFilters: [{ Filter: 'paddock-*', FilterType: 'WILDCARD' }] },
    ]);
  });

  test('the image action no longer sets the deprecated repository flag', () => {
    const action = readFileSync(resolve(REPO_ROOT, '.github/actions/image/action.yml'), 'utf-8');
    assert.doesNotMatch(action, /image-scanning-configuration|scanOnPush/);
  });
});
