/**
 * The main stack, infra/paddock.cfn.yml: the listener as one Fargate Spot
 * service whose health check is the listener's own heartbeat, what the
 * listener needs to start runner tasks, and the alarm that pages when it is
 * down (docs/constraints/deploy.md).
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { loadTemplate, onlyResource, resourcesOfType, type Statement } from './helpers/cfn.ts';

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

describe('the listener-down alarm', () => {
  const alarms = resourcesOfType(tpl, 'AWS::CloudWatch::Alarm');
  const down = alarms.find(([id]) => id === 'ListenerDownAlarm')?.[1];

  test('fires when the service reports no task, and when it reports nothing at all', () => {
    assert.equal(down?.Properties?.Namespace, 'AWS/ECS');
    assert.equal(down?.Properties?.Statistic, 'SampleCount');
    assert.equal(down?.Properties?.ComparisonOperator, 'LessThanThreshold');
    assert.equal(down?.Properties?.Threshold, 1);
    assert.equal(down?.Properties?.TreatMissingData, 'breaching');
    assert.deepEqual(down?.Properties?.Dimensions, [
      { Name: 'ClusterName', Value: '${Cluster}' },
      { Name: 'ServiceName', Value: '${ListenerService.Name}' },
    ]);
  });

  test('pages the operator through the alerts topic', () => {
    assert.deepEqual(down?.Properties?.AlarmActions, ['${AlertsTopic}']);
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
// comes, so the listener has no task to stop.
describe('fitness: the listener stops no task (remove-then-terminate.md)', () => {
  test('no role may stop a task', () => {
    for (const [id] of Object.entries(roles)) {
      for (const s of statementsOf(id)) assert.ok(!actionsOf(s).includes('ecs:StopTask'), `${id} can stop a task`);
    }
  });
});
