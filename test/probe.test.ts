/**
 * The deploy's probe judges whether the listener service runs, healthy, the
 * image the deploy applied (docs/constraints/deploy.md). Its exit code is
 * its whole contract with the deploy: 0 keeps the image, 1 rolls it back.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { CLUSTER, SERVICE, judge, probe, type Observed } from '../scripts/probe.mjs';
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

  test('judges the primary deployment, wherever ECS lists it', () => {
    const o = observed({
      deployments: [
        { status: 'ACTIVE', taskDefinition: 'td:1', rolloutState: 'COMPLETED' },
        { status: 'PRIMARY', taskDefinition: 'td:2', rolloutState: 'IN_PROGRESS' },
      ],
      images: { 'td:1': OLD, 'td:2': NEW },
    });
    assert.equal(judge(o, NEW).verdict, 'wait');
  });

  test('is not satisfied by a healthy task that is stopping', () => {
    const o = observed({ tasks: [{ taskDefinitionArn: 'td:2', lastStatus: 'DEACTIVATING', healthStatus: 'HEALTHY' }] });
    assert.equal(judge(o, NEW).verdict, 'wait');
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

describe('the probe\'s exit code', () => {
  // A clock that moves only when the probe sleeps, so no test waits.
  const run = async (reads: Array<Observed | Error>, timeoutMs = 60_000) => {
    let clock = 0;
    let call = 0;
    const lines: string[] = [];
    const code = await probe({
      expected: NEW,
      timeoutMs,
      observe: async () => {
        const next = reads[Math.min(call++, reads.length - 1)];
        if (next instanceof Error) throw next;
        return next!;
      },
      now: () => clock,
      sleep: async (ms: number) => { clock += ms; },
      log: (line: string) => lines.push(line),
    });
    return { code, calls: call, lines };
  };
  const waiting = observed({ deployments: [{ status: 'PRIMARY', taskDefinition: 'td:2', rolloutState: 'IN_PROGRESS' }] });

  test('is 0 once the listener passes, after waiting for it', async () => {
    const { code, calls } = await run([waiting, waiting, observed()]);
    assert.equal(code, 0);
    assert.equal(calls, 3);
  });

  test('is 1 at once on a failed verdict, without waiting out its time', async () => {
    const { code, calls } = await run([observed({ images: { 'td:2': OLD } })]);
    assert.equal(code, 1);
    assert.equal(calls, 1);
  });

  test('is 1 when its time runs out while it waits', async () => {
    const { code, lines } = await run([waiting], 60_000);
    assert.equal(code, 1);
    assert.ok(lines.some((l) => /timed out/.test(l)));
  });

  // A throttled or failed read says nothing about the image, so it must not
  // roll a good image back.
  test('waits through a failed read of the service', async () => {
    const { code } = await run([new Error('ThrottlingException'), observed()]);
    assert.equal(code, 0);
  });

  test('is 1 when reads fail until its time runs out', async () => {
    const { code, lines } = await run([new Error('ThrottlingException')], 60_000);
    assert.equal(code, 1);
    assert.ok(lines.some((l) => /ThrottlingException/.test(l)));
  });
});
