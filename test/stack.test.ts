/**
 * The main stack, infra/paddock.cfn.yml: the listener as one Fargate Spot
 * service whose health check is the listener's own heartbeat, what the
 * listener needs to start runner tasks, and the alarms that page the
 * operator (docs/constraints/deploy.md).
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { loadTemplate, onlyResource, resourcesOfType, REPO_ROOT, type Statement } from './helpers/cfn.ts';

const tpl = loadTemplate('infra/paddock.cfn.yml');
const service = onlyResource(tpl, 'AWS::ECS::Service');
const taskDef = onlyResource(tpl, 'AWS::ECS::TaskDefinition');
const container = taskDef.Properties?.ContainerDefinitions?.[0];
const env: Record<string, unknown> = Object.fromEntries(
  (container?.Environment ?? []).map((e: { Name: string; Value: unknown }) => [e.Name, e.Value]),
);
const roles = Object.fromEntries(resourcesOfType(tpl, 'AWS::IAM::Role'));
const statementsOf = (id: string): Statement[] =>
  (roles[id]?.Properties?.Policies ?? []).flatMap((p: { PolicyDocument?: { Statement?: Statement[] } }) => p.PolicyDocument?.Statement ?? []);
const actionsOf = (s: Statement): string[] => [s.Action].flat().map(String);
const resourcesOf = (s: Statement): string[] => [s.Resource].flat().map(String);

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

  // GitHub allows one message session per scale set, so a new listener
  // started beside the old one would be refused until the old one stopped.
  test('fitness: the old listener task stops before the new one starts (deploy.md)', () => {
    assert.equal(service.Properties?.DeploymentConfiguration?.MinimumHealthyPercent, 0);
    assert.equal(service.Properties?.DeploymentConfiguration?.MaximumPercent, 100);
  });

  // ECS turns rebalancing on by default, and refuses to update a service
  // that has it on and a maximum of 100% or less.
  test('turns off zone rebalancing, which ECS refuses beside a maximum of 100%', () => {
    assert.equal(service.Properties?.AvailabilityZoneRebalancing, 'DISABLED');
  });

  test('gives the listener two minutes, Fargate Spot\'s notice, to close its sessions on SIGTERM', () => {
    assert.equal(container?.StopTimeout, 120);
  });

  test('is healthy only while the listener\'s heartbeat is fresh', () => {
    assert.deepEqual(container?.HealthCheck?.Command, ['CMD', '/listener', '-healthcheck']);
  });

  test('runs the image the deploy names, on arm64, and logs its digest rather than its registry', () => {
    assert.equal(container?.Image, '${ImageUri}');
    assert.equal(taskDef.Properties?.RuntimePlatform?.CpuArchitecture, 'ARM64');
    assert.equal(env.PADDOCK_IMAGE_DIGEST, '${Select}', 'the digest is the part of ImageUri after its @');
    assert.ok(!Object.values(env).includes('${ImageUri}'), 'the image URI carries the account id');
  });

  test('tells the listener where and how to start runner tasks', () => {
    assert.deepEqual(env, {
      PADDOCK_IMAGE_DIGEST: '${Select}',
      PADDOCK_CLUSTER: '${Cluster}',
      PADDOCK_SUBNETS: '${Join}',
      PADDOCK_RUNNER_SECURITY_GROUP: '${RunnerSecurityGroup}',
      PADDOCK_RUNNER_IMAGE: '${RunnerImageUri}',
      PADDOCK_RUNNER_EXECUTION_ROLE_ARN: '${RunnerExecutionRole.Arn}',
      PADDOCK_RUNNER_LOG_GROUP: '${RunnerLogs}',
      PADDOCK_CONFIG_BUCKET: '${RunnerConfigs}',
      AWS_REGION: '${AWS::Region}',
    });
  });

  test('takes the runner\'s image as a second image parameter, pinned by digest', () => {
    assert.equal(tpl.Parameters?.RunnerImageUri?.Type, 'String');
    assert.equal(tpl.Parameters?.RunnerImageUri?.AllowedPattern, tpl.Parameters?.ImageUri?.AllowedPattern);
  });

  test('reaches GitHub and AWS through a public IP in the default VPC, with no NAT to pay for', () => {
    assert.equal(service.Properties?.NetworkConfiguration?.AwsvpcConfiguration?.AssignPublicIp, 'ENABLED');
  });

  test('takes no inbound traffic', () => {
    for (const [, group] of resourcesOfType(tpl, 'AWS::EC2::SecurityGroup')) {
      assert.equal(group.Properties?.SecurityGroupIngress, undefined);
    }
  });

  test('keeps its logs, and the runners\', for a bounded time', () => {
    const groups = resourcesOfType(tpl, 'AWS::Logs::LogGroup');
    assert.deepEqual(groups.map(([id]) => id).sort(), ['ListenerLogs', 'RunnerLogs']);
    for (const [, group] of groups) assert.ok(Number(group.Properties?.RetentionInDays) > 0);
  });
});

describe('the stack\'s grants', () => {
  test('the execution roles hold ECS\'s task-execution policy, and the listener\'s own role no managed policy', () => {
    assert.deepEqual(roles.ListenerTaskRole?.Properties?.ManagedPolicyArns ?? [], []);
    for (const id of ['ListenerExecutionRole', 'RunnerExecutionRole']) {
      assert.deepEqual(roles[id]?.Properties?.ManagedPolicyArns, [
        'arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy',
      ], id);
    }
    assert.deepEqual(roles.ListenerExecutionRole?.Properties?.Policies ?? [], []);
  });

  test('no role holds an administrator policy or a statement allowing every action', () => {
    assert.ok(Object.keys(roles).length > 0);
    for (const [id, role] of Object.entries(roles)) {
      for (const arn of role.Properties?.ManagedPolicyArns ?? []) {
        assert.doesNotMatch(String(arn), /AdministratorAccess|PowerUserAccess/, id);
      }
      const statements: Statement[] = (role.Properties?.Policies ?? []).flatMap((p) => p.PolicyDocument?.Statement ?? []);
      for (const s of statements) assert.ok(![s.Action].flat().includes('*'), `${id} allows every action`);
    }
  });
});

describe('fitness: the alert address never passes through GitHub (public-repo.md)', () => {
  test('the stack reads it from Parameter Store, and the alerts topic mails it', () => {
    assert.equal(tpl.Parameters?.AlertEmail?.Type, 'AWS::SSM::Parameter::Value<String>');
    assert.equal(tpl.Parameters?.AlertEmail?.Default, '/paddock/alert-email');
    assert.deepEqual(tpl.Resources.AlertsTopic?.Properties?.Subscription, [{ Protocol: 'email', Endpoint: '${AlertEmail}' }]);
  });
});

// Each alarm counts a line the listener logs, through a metric filter on
// its log group, and pages the operator through the alerts topic.
describe('the alarms', () => {
  const alarms = Object.fromEntries(resourcesOfType(tpl, 'AWS::CloudWatch::Alarm'));
  const filters = resourcesOfType(tpl, 'AWS::Logs::MetricFilter').map(([, f]) => f.Properties ?? {});
  const filterOf = (metric: string) => filters.find((f) => f.MetricTransformations?.[0]?.MetricName === metric);
  // The messages the listener logs, from its Go source.
  const goSource = ['internal/runners/recover.go', 'cmd/listener/main.go']
    .map((f) => readFileSync(resolve(REPO_ROOT, f), 'utf-8')).join('\n');
  const logged = (msg: string) => goSource.includes(`"${msg}"`);
  const msgOf = (pattern: string) => /\$\.msg = "([^"]+)"/.exec(pattern)?.[1] ?? '';

  const cases: [alarm: string, metric: string, pattern: string][] = [
    ['ListenerDownAlarm', 'ListenerPolls', '{ $.msg = "listener polling" }'],
    ['JobWaitingAlarm', 'JobsWaitingLong', '{ $.msg = "job waiting for a runner" }'],
    ['FailedStartsAlarm', 'FailedStartsInARow', '{ $.msg = "failed start" && $.inARow >= 3 }'],
    ['TaskPastMaxJobAlarm', 'TasksPastMaxJob', '{ $.msg = "task past its pool\'s maximum job time" }'],
    ['SpotKillRateAlarm', 'SpotKillsOverLimit', '{ $.msg = "spot kills over their limit" }'],
  ];
  for (const [id, metric, pattern] of cases) {
    test(`${id} counts the listener's "${msgOf(pattern)}" lines and pages the operator`, () => {
      const alarm = alarms[id]?.Properties;
      assert.ok(alarm, `no ${id}`);
      assert.equal(alarm.Namespace, 'paddock');
      assert.equal(alarm.MetricName, metric);
      assert.deepEqual(alarm.AlarmActions, ['${AlertsTopic}']);
      assert.deepEqual(alarm.OKActions, ['${AlertsTopic}']);
      const filter = filterOf(metric);
      assert.equal(filter?.LogGroupName, '${ListenerLogs}');
      assert.equal(filter?.FilterPattern, pattern);
      assert.equal(filter?.MetricTransformations?.[0]?.MetricNamespace, 'paddock');
      assert.ok(logged(msgOf(pattern)), `the listener never logs "${msgOf(pattern)}"`);
    });
  }

  test('the listener-down alarm fires when the listener logs no poll for five minutes, running or not', () => {
    const down = alarms.ListenerDownAlarm?.Properties;
    assert.equal(down?.Statistic, 'Sum');
    assert.equal(down?.ComparisonOperator, 'LessThanThreshold');
    assert.equal(down?.Threshold, 1);
    assert.equal(down?.Period * down?.EvaluationPeriods, 300);
    assert.equal(down?.TreatMissingData, 'breaching');
  });

  test('the other alarms fire on any line, and treat none as all is well', () => {
    for (const id of ['JobWaitingAlarm', 'FailedStartsAlarm', 'TaskPastMaxJobAlarm', 'SpotKillRateAlarm']) {
      const a = alarms[id]?.Properties;
      assert.equal(a?.Statistic, 'Sum', id);
      assert.equal(a?.ComparisonOperator, 'GreaterThanOrEqualToThreshold', id);
      assert.equal(a?.Threshold, 1, id);
      assert.equal(a?.TreatMissingData, 'notBreaching', id);
    }
  });

  // The listener logs the rate a little over an hour apart, so an hour can
  // pass with no line while the rate stays over its limit: the alarm looks
  // back two hours and fires on one line, so it does not flap.
  test('the Spot-kill alarm holds through an hour with no line', () => {
    const a = alarms.SpotKillRateAlarm?.Properties;
    assert.equal(a?.Period, 3600);
    assert.equal(a?.EvaluationPeriods, 2);
    assert.equal(a?.DatapointsToAlarm, 1);
  });

  test('each pool\'s finished, Spot-killed, warned and re-run jobs are counted by repo and pool', () => {
    for (const [metric, msg] of [
      ['JobsFinished', 'job finished'],
      ['JobsKilledBySpot', 'job killed by a Spot reclaim'],
      ['JobsFinishedInWarning', 'job finished inside the warning'],
      ['JobsReRun', 'job re-run'],
    ]) {
      const f = filterOf(metric!);
      assert.equal(f?.FilterPattern, `{ $.msg = "${msg}" }`, metric);
      assert.deepEqual(f?.MetricTransformations?.[0]?.Dimensions, [
        { Key: 'Repo', Value: '$.repo' },
        { Key: 'Pool', Value: '$.pool' },
      ], metric);
      assert.ok(logged(msg!), `the listener never logs "${msg}"`);
    }
  });

  test('the listener reads its pools\' job counts back, for the 30 days no alarm can look over', () => {
    const read = statementsOf('ListenerTaskRole').filter((s) => actionsOf(s).some((a) => a.startsWith('cloudwatch:')));
    assert.deepEqual(read.map(actionsOf), [['cloudwatch:GetMetricData']]);
  });
});

// Only the listener holds the App key and starts or stops tasks, and a
// runner task reads only its own config.
describe('fitness: only the listener reads the App key and starts tasks, and a runner reads only its own config (runner-isolation.md)', () => {
  const listener = statementsOf('ListenerTaskRole');
  const parameter = (path: string) => `arn:aws:ssm:\${AWS::Region}:\${AWS::AccountId}:parameter${path}`;

  test('the listener reads the App key and the repo list, and no other parameter or secret of the operator\'s', () => {
    const reads = listener.filter((s) => actionsOf(s).some((a) => /^ssm:GetParameters?$|^secretsmanager:/.test(a)));
    assert.deepEqual(reads.flatMap(resourcesOf).sort(), [parameter('/paddock/github-app'), parameter('/paddock/repos')].sort());
    for (const s of reads) assert.deepEqual(actionsOf(s), ['ssm:GetParameter']);
  });

  test('the listener writes, lists and deletes runner configs under /paddock/runners alone', () => {
    const writes = listener.filter((s) => actionsOf(s).some((a) => /^ssm:(Put|Delete|GetParametersByPath)/.test(a)));
    assert.deepEqual(writes.flatMap(resourcesOf), [parameter('/paddock/runners/*')]);
  });

  test('the listener starts runner tasks only from the runner family, in this stack\'s cluster', () => {
    const run = listener.find((s) => actionsOf(s).includes('ecs:RunTask'));
    assert.deepEqual(resourcesOf(run!), ['arn:aws:ecs:${AWS::Region}:${AWS::AccountId}:task-definition/paddock-runner:*']);
    assert.deepEqual(run?.Condition, { ArnEquals: { 'ecs:cluster': '${Cluster.Arn}' } });
  });

  test('the listener passes the runner\'s execution role to ECS, and no other role', () => {
    const pass = listener.filter((s) => actionsOf(s).includes('iam:PassRole'));
    assert.deepEqual(pass.flatMap(resourcesOf), ['${RunnerExecutionRole.Arn}']);
  });

  test('no other role can start or stop a task, or read the App key', () => {
    for (const [id] of Object.entries(roles)) {
      if (id === 'ListenerTaskRole') continue;
      for (const s of statementsOf(id)) {
        assert.ok(!actionsOf(s).some((a) => /^ecs:(RunTask|StartTask|StopTask)$/.test(a)), `${id} can start or stop a task`);
        assert.ok(!resourcesOf(s).some((r) => r.includes('github-app')), `${id} can read the App key`);
      }
    }
  });

  test('the runner\'s execution role reads runner configs alone, for ECS to inject each into its own task', () => {
    const statements = statementsOf('RunnerExecutionRole');
    assert.deepEqual(statements.map(actionsOf), [['ssm:GetParameters']]);
    assert.deepEqual(statements.flatMap(resourcesOf), [parameter('/paddock/runners/*')]);
  });

  test('the listener writes, reads, deletes and lists the runner-config bucket, and no other bucket', () => {
    const s3 = listener.filter((s) => actionsOf(s).some((a) => a.startsWith('s3:')));
    const grants = s3.map((s) => [actionsOf(s).sort(), resourcesOf(s)]);
    assert.deepEqual(grants, [
      [['s3:DeleteObject', 's3:GetObject', 's3:PutObject'], ['${RunnerConfigs.Arn}/*']],
      [['s3:ListBucket'], ['${RunnerConfigs.Arn}']],
    ]);
  });

  test('no other role reaches a bucket: a running task reaches its own object through its URL alone', () => {
    for (const [id] of Object.entries(roles)) {
      if (id === 'ListenerTaskRole') continue;
      for (const s of statementsOf(id)) assert.ok(!actionsOf(s).some((a) => a.startsWith('s3:')), `${id} reaches S3`);
    }
  });

  test('the runner-config bucket blocks public access, expires objects after a day, and takes TLS alone', () => {
    const bucket = onlyResource(tpl, 'AWS::S3::Bucket');
    assert.equal(tpl.Resources.RunnerConfigs, bucket);
    assert.deepEqual(bucket.Properties?.PublicAccessBlockConfiguration, {
      BlockPublicAcls: true,
      BlockPublicPolicy: true,
      IgnorePublicAcls: true,
      RestrictPublicBuckets: true,
    });
    const rules = bucket.Properties?.LifecycleConfiguration?.Rules ?? [];
    assert.ok(rules.some((r: { Status?: string; ExpirationInDays?: number; Prefix?: string }) =>
      r.Status === 'Enabled' && r.ExpirationInDays === 1 && !r.Prefix), JSON.stringify(rules));
    const policy = onlyResource(tpl, 'AWS::S3::BucketPolicy');
    const deny = (policy.Properties?.PolicyDocument?.Statement ?? []).find((s: Statement) => s.Effect === 'Deny');
    assert.deepEqual(deny?.Condition, { Bool: { 'aws:SecureTransport': 'false' } });
  });

  test('the stack defines no runner task definition or task role: the listener registers each runner\'s own', () => {
    assert.deepEqual(resourcesOfType(tpl, 'AWS::ECS::TaskDefinition').map(([id]) => id), ['ListenerTaskDefinition']);
    assert.deepEqual(Object.keys(roles).sort(), ['ListenerExecutionRole', 'ListenerTaskRole', 'RunnerExecutionRole']);
  });
});

describe('fitness: the stack runs in us-east-1 alone (region.md)', () => {
  test('a rule refuses any other region', () => {
    const rules = (tpl as { Rules?: Record<string, { Assertions?: Array<{ Assert?: unknown }> }> }).Rules;
    assert.equal(rules?.UsEast1Only?.Assertions?.[0]?.Assert, '${Equals:${AWS::Region},us-east-1}');
  });
});

// GitHub holds the queue: jobs wait in GitHub until a pool has room, and
// the listener polls for them. Nothing in the account takes a webhook or
// holds a queue of its own.
describe('fitness: GitHub is the queue, with no webhook and no queue of our own (github-queue.md)', () => {
  const stacks = ['infra/bootstrap.cfn.yml', 'infra/paddock.cfn.yml'].map((file) => [file, loadTemplate(file)] as const);

  test('no stack defines a queue, a stream, or anything that takes a request from outside', () => {
    const refused = /^AWS::(SQS|Kinesis|Events|Pipes|Lambda|ApiGateway|ApiGatewayV2|ElasticLoadBalancing|ElasticLoadBalancingV2|AppRunner|CloudFront)::/;
    for (const [file, stack] of stacks) {
      const found = Object.entries(stack.Resources).filter(([, r]) => refused.test(r.Type));
      assert.deepEqual(found.map(([id, r]) => `${id} ${r.Type}`), [], file);
    }
  });

  test('no security group lets anything in', () => {
    for (const [file, stack] of stacks) {
      for (const [id, group] of resourcesOfType(stack, 'AWS::EC2::SecurityGroup')) {
        assert.equal(group.Properties?.SecurityGroupIngress, undefined, `${file} ${id}`);
      }
      assert.deepEqual(resourcesOfType(stack, 'AWS::EC2::SecurityGroupIngress'), [], file);
    }
  });
});

// Which repos are served is the operator's business, kept in the CI
// account: the listener reads it at start, and no stack writes it.
describe('fitness: served repos are configuration in the CI account, never a file here (served-repos.md)', () => {
  test('the listener reads the list from Parameter Store, /paddock/repos', () => {
    const reads = statementsOf('ListenerTaskRole').filter((s) => actionsOf(s).includes('ssm:GetParameter'));
    assert.ok(reads.flatMap(resourcesOf).some((r) => r.endsWith(':parameter/paddock/repos')));
  });

  test('no stack creates a parameter', () => {
    for (const file of ['infra/bootstrap.cfn.yml', 'infra/paddock.cfn.yml']) {
      assert.deepEqual(resourcesOfType(loadTemplate(file), 'AWS::SSM::Parameter'), [], file);
    }
  });

  // The bootstrap stack takes this repo's own name, for the deploy role's
  // trust; the main stack takes no repo at all.
  test('the main stack takes no repo as a parameter', () => {
    for (const name of Object.keys(tpl.Parameters ?? {})) assert.doesNotMatch(name, /repo/i, name);
  });
});

// A task ends itself once its runner has left GitHub and no new config
// comes. The listener stops a task only when its runner never took its job,
// and only a runner task in its own cluster.
describe('fitness: the listener stops only runner tasks whose runners never took their jobs (remove-then-terminate.md)', () => {
  test('the listener may stop tasks in its own cluster alone, and no other role may stop any', () => {
    for (const [id] of Object.entries(roles)) {
      const stops = statementsOf(id).filter((s) => actionsOf(s).includes('ecs:StopTask'));
      if (id !== 'ListenerTaskRole') {
        assert.deepEqual(stops, [], `${id} can stop a task`);
        continue;
      }
      assert.equal(stops.length, 1);
      assert.deepEqual(actionsOf(stops[0]!), ['ecs:StopTask']);
      assert.deepEqual(resourcesOf(stops[0]!), ['arn:aws:ecs:${AWS::Region}:${AWS::AccountId}:task/paddock/*']);
      assert.deepEqual(stops[0]!.Condition, { ArnEquals: { 'ecs:cluster': '${Cluster.Arn}' } });
    }
  });
});
