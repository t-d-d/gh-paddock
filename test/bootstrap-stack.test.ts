/**
 * The bootstrap stack: the role a run on main assumes to deploy
 * (docs/constraints/deploy.md). The App key is no part of a stack
 * (docs/constraints/public-repo.md).
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { loadTemplate } from './helpers/cfn.ts';

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
  for (const file of ['infra/bootstrap.cfn.yml', 'infra/paddock.cfn.yml']) {
    const resources = Object.entries(loadTemplate(file).Resources);

    test(`${file} defines no Secrets Manager secret`, () => {
      const secrets = resources.filter(([, r]) => String(r?.Type).startsWith('AWS::SecretsManager::'));
      assert.deepEqual(secrets.map(([name]) => name), []);
    });

    test(`${file} defines no parameter for the App key`, () => {
      const parameters = resources.filter(
        ([, r]) => r?.Type === 'AWS::SSM::Parameter' && JSON.stringify(r?.Properties?.Name ?? '').includes('github-app'),
      );
      assert.deepEqual(parameters.map(([name]) => name), []);
    });
  }
});
