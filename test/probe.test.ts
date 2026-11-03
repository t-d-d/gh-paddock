/**
 * The deploy's probe judges whether the listener service runs, healthy, the
 * image the deploy applied (docs/constraints/deploy.md). These tests pin the
 * judgment on what ECS reports; the loop that polls ECS is a thin shell
 * around it.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { CLUSTER, SERVICE, judge, type Observed } from '../scripts/probe.mjs';
import { loadTemplate, onlyResource } from './helpers/cfn.ts';

const NEW = 'example.registry/paddock@sha256:' + 'a'.repeat(64);
const OLD = 'example.registry/paddock@sha256:' + 'b'.repeat(64);

const observed = (over: Partial<Observed> = {}): Observed => ({
  deployments: [{ status: 'PRIMARY', taskDefinition: 'td:2', rolloutState: 'COMPLETED' }],
  images: { 'td:2': NEW },
  tasks: [{ taskDefinitionArn: 'td:2', lastStatus: 'RUNNING', healthStatus: 'HEALTHY' }],
  ...over,
});

describe('the probe', () => {
  test('passes when the rollout is complete and a task of the new image is healthy', () => {
    assert.equal(judge(observed(), NEW).verdict, 'pass');
  });

  test('waits while the rollout is in progress', () => {
    const o = observed({ deployments: [{ status: 'PRIMARY', taskDefinition: 'td:2', rolloutState: 'IN_PROGRESS' }] });
    assert.equal(judge(o, NEW).verdict, 'wait');
  });

  test('waits while the new task has not reported healthy', () => {
    const o = observed({ tasks: [{ taskDefinitionArn: 'td:2', lastStatus: 'RUNNING', healthStatus: 'UNKNOWN' }] });
    assert.equal(judge(o, NEW).verdict, 'wait');
  });

  test('is not satisfied by a healthy task of the old task definition', () => {
    const o = observed({ tasks: [{ taskDefinitionArn: 'td:1', lastStatus: 'RUNNING', healthStatus: 'HEALTHY' }] });
    assert.equal(judge(o, NEW).verdict, 'wait');
  });

  test('fails when ECS marks the rollout failed', () => {
    const o = observed({ deployments: [{ status: 'PRIMARY', taskDefinition: 'td:2', rolloutState: 'FAILED' }] });
    assert.equal(judge(o, NEW).verdict, 'fail');
  });

  test('fails when the service runs another image, as after ECS rolled it back', () => {
    const o = observed({ images: { 'td:2': OLD } });
    const result = judge(o, NEW);
    assert.equal(result.verdict, 'fail');
    assert.match(result.reason, /sha256:b{64}/);
    assert.doesNotMatch(result.reason, /registry/, 'the reason names the digest, not the registry, which carries the account id');
  });

  test('fails when the service has no primary deployment', () => {
    assert.equal(judge(observed({ deployments: [] }), NEW).verdict, 'fail');
  });

  test('probes the cluster and service the stack names', () => {
    const tpl = loadTemplate('infra/paddock.cfn.yml');
    assert.equal(onlyResource(tpl, 'AWS::ECS::Cluster').Properties?.ClusterName, CLUSTER);
    assert.equal(onlyResource(tpl, 'AWS::ECS::Service').Properties?.ServiceName, SERVICE);
  });
});
