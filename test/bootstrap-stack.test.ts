/**
 * The bootstrap stack: the role a run on main assumes to deploy, and the
 * secret the operator writes before the first deploy
 * (docs/constraints/deploy.md).
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

  test('the App key\'s secret outlives a template change, and is created holding only a placeholder', () => {
    const secret = tpl.Resources.GitHubAppSecret;
    assert.equal(secret?.Type, 'AWS::SecretsManager::Secret');
    assert.equal(secret?.Properties?.Name, 'paddock/github-app');
    assert.equal(secret?.DeletionPolicy, 'RetainExceptOnCreate');
    assert.equal(secret?.UpdateReplacePolicy, 'Retain');
    assert.equal(secret?.Properties?.SecretString, undefined, 'a value in the template would be public');
  });

  test('hands the deploy role\'s ARN out, for the AWS_DEPLOY_ROLE_ARN secret', () => {
    assert.equal(tpl.Outputs?.DeployRoleArn?.Value, '${DeployRole.Arn}');
  });
});
