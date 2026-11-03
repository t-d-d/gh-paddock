/**
 * The main stack, infra/paddock.cfn.yml. For now it runs the walking
 * skeleton: the listener as one Fargate Spot service whose health check is
 * the listener's own heartbeat, and the alarm that pages when it is down
 * (docs/constraints/deploy.md).
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { loadTemplate, onlyResource, resourcesOfType, type Statement } from './helpers/cfn.ts';

const tpl = loadTemplate('infra/paddock.cfn.yml');
const service = onlyResource(tpl, 'AWS::ECS::Service');
const taskDef = onlyResource(tpl, 'AWS::ECS::TaskDefinition');
const container = taskDef.Properties?.ContainerDefinitions?.[0];

describe('the listener service', () => {
  test('runs one task on Fargate Spot, in a cluster that offers Spot and on-demand', () => {
    assert.equal(service.Properties?.DesiredCount, 1);
    assert.deepEqual(service.Properties?.CapacityProviderStrategy, [{ CapacityProvider: 'FARGATE_SPOT', Weight: 1 }]);
    const cluster = onlyResource(tpl, 'AWS::ECS::Cluster');
    assert.deepEqual([...(cluster.Properties?.CapacityProviders ?? [])].sort(), ['FARGATE', 'FARGATE_SPOT']);
  });

  test('rolls itself back when a new task never turns healthy, so a deploy cannot hang on one', () => {
    assert.deepEqual(service.Properties?.DeploymentConfiguration?.DeploymentCircuitBreaker, { Enable: true, Rollback: true });
  });

  test('keeps the old task until the new one is healthy', () => {
    assert.equal(service.Properties?.DeploymentConfiguration?.MinimumHealthyPercent, 100);
    assert.equal(service.Properties?.DeploymentConfiguration?.MaximumPercent, 200);
  });

  test('is healthy only while the listener\'s heartbeat is fresh', () => {
    assert.deepEqual(container?.HealthCheck?.Command, ['CMD', '/listener', '-healthcheck']);
  });

  test('runs the image the deploy names, on arm64, and logs its digest rather than its registry', () => {
    assert.equal(container?.Image, '${ImageUri}');
    assert.equal(taskDef.Properties?.RuntimePlatform?.CpuArchitecture, 'ARM64');
    const env = Object.fromEntries((container?.Environment ?? []).map((e: { Name: string; Value: unknown }) => [e.Name, e.Value]));
    assert.equal(env.PADDOCK_IMAGE_DIGEST, '${Select}', 'the digest is the part of ImageUri after its @');
    assert.ok(!Object.values(env).includes('${ImageUri}'), 'the image URI carries the account id');
  });

  test('takes no inbound traffic', () => {
    for (const [, group] of resourcesOfType(tpl, 'AWS::EC2::SecurityGroup')) {
      assert.equal(group.Properties?.SecurityGroupIngress, undefined);
    }
  });

  test('keeps its logs for a bounded time', () => {
    const logs = onlyResource(tpl, 'AWS::Logs::LogGroup');
    assert.ok(Number(logs.Properties?.RetentionInDays) > 0);
  });
});

describe('the stack\'s grants', () => {
  test('no role is allowed every action', () => {
    const statements: Statement[] = resourcesOfType(tpl, 'AWS::IAM::Role').flatMap(([, role]) => [
      ...(role.Properties?.Policies ?? []).flatMap((p) => p.PolicyDocument?.Statement ?? []),
    ]);
    for (const s of statements) {
      assert.ok(![s.Action].flat().includes('*'), `a statement allows every action: ${JSON.stringify(s)}`);
    }
  });
});

describe('the listener-down alarm', () => {
  const alarms = resourcesOfType(tpl, 'AWS::CloudWatch::Alarm');
  const down = alarms.find(([id]) => id === 'ListenerDownAlarm')?.[1];

  test('fires when the service reports no task, and when it reports nothing at all', () => {
    assert.equal(down?.Properties?.Namespace, 'AWS/ECS');
    assert.equal(down?.Properties?.Statistic, 'SampleCount');
    assert.equal(down?.Properties?.ComparisonOperator, 'LessThanThreshold');
    assert.equal(down?.Properties?.Threshold, 1);
    assert.equal(down?.Properties?.TreatMissingData, 'breaching');
  });

  test('pages the operator through the alerts topic, whose address never passes through GitHub', () => {
    assert.deepEqual(down?.Properties?.AlarmActions, ['${AlertsTopic}']);
    assert.equal(tpl.Parameters?.AlertEmail?.Type, 'AWS::SSM::Parameter::Value<String>');
    assert.equal(tpl.Parameters?.AlertEmail?.Default, '/paddock/alert-email');
  });
});
