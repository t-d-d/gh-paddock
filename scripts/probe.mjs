#!/usr/bin/env node
/**
 * The deploy's probe (docs/constraints/deploy.md): does the listener service
 * run, healthy, the image this deploy applied? It polls ECS until the
 * service's primary deployment has finished rolling out and a task of it
 * reports healthy, and exits 0; it exits 1 on a failed rollout, on a service
 * running another image, or when its time runs out.
 *
 * Healthy means the listener's own heartbeat is fresh (`listener
 * -healthcheck`), so the probe proves the new image runs and beats. It names
 * images by digest only: the registry in an image URI carries the account id.
 *
 *   EXPECTED_IMAGE_URI     the image the deploy applied; unset, the image
 *                          the stack runs now (a probe-only run)
 *   PROBE_TIMEOUT_SECONDS  how long to wait, 600 by default
 *
 *   node scripts/probe.mjs
 */

import { CloudFormationClient, DescribeStacksCommand } from '@aws-sdk/client-cloudformation';
import {
  DescribeServicesCommand,
  DescribeTaskDefinitionCommand,
  DescribeTasksCommand,
  ECSClient,
  ListTasksCommand,
} from '@aws-sdk/client-ecs';
import { isCliEntry } from './lib/cli-entry.mjs';

export const STACK = 'paddock';
export const CLUSTER = 'paddock';
export const SERVICE = 'listener';

/**
 * @typedef {{ status?: string | undefined, taskDefinition?: string | undefined, rolloutState?: string | undefined }} Deployment
 * @typedef {{ taskDefinitionArn?: string | undefined, lastStatus?: string | undefined, healthStatus?: string | undefined }} Task
 * @typedef {{ deployments: Deployment[], images: Record<string, string | undefined>, tasks: Task[] }} Observed
 * @typedef {{ verdict: 'pass' | 'wait' | 'fail', reason: string }} Judgment
 */

/** An image URI's digest, the part after its `@`: safe to print. */
export const digestOf = (/** @type {string | undefined} */ uri) => (uri ?? '').split('@')[1] ?? '(no digest)';

/**
 * What ECS reports, judged against the image the deploy applied.
 *
 * @param {Observed} observed
 * @param {string} expectedImage
 * @returns {Judgment}
 */
export function judge({ deployments, images, tasks }, expectedImage) {
  const primary = deployments.find((d) => d.status === 'PRIMARY');
  if (!primary?.taskDefinition) return { verdict: 'fail', reason: 'the service has no primary deployment' };
  const image = images[primary.taskDefinition];
  if (image !== expectedImage) {
    return {
      verdict: 'fail',
      reason: `the service runs ${digestOf(image)}, not ${digestOf(expectedImage)}: ECS rolled the deploy back, or another apply replaced it`,
    };
  }
  if (primary.rolloutState === 'FAILED') return { verdict: 'fail', reason: 'ECS marked the rollout failed' };
  if (primary.rolloutState !== 'COMPLETED') return { verdict: 'wait', reason: `the rollout is ${primary.rolloutState ?? 'in an unknown state'}` };
  const healthy = tasks.some(
    (t) => t.taskDefinitionArn === primary.taskDefinition && t.lastStatus === 'RUNNING' && t.healthStatus === 'HEALTHY',
  );
  if (!healthy) return { verdict: 'wait', reason: 'no task of the new image reports healthy yet' };
  return { verdict: 'pass', reason: `the listener runs ${digestOf(expectedImage)}, healthy` };
}

/**
 * Read what ECS reports about the listener service.
 *
 * @param {ECSClient} ecs
 * @returns {Promise<Observed>}
 */
async function observe(ecs) {
  const { services = [] } = await ecs.send(new DescribeServicesCommand({ cluster: CLUSTER, services: [SERVICE] }));
  const deployments = services[0]?.deployments ?? [];
  /** @type {Record<string, string | undefined>} */
  const images = {};
  for (const arn of new Set(deployments.map((d) => d.taskDefinition).filter((a) => a !== undefined))) {
    const { taskDefinition } = await ecs.send(new DescribeTaskDefinitionCommand({ taskDefinition: arn }));
    images[arn] = taskDefinition?.containerDefinitions?.[0]?.image;
  }
  const { taskArns = [] } = await ecs.send(new ListTasksCommand({ cluster: CLUSTER, serviceName: SERVICE, desiredStatus: 'RUNNING' }));
  const tasks = taskArns.length === 0 ? [] : ((await ecs.send(new DescribeTasksCommand({ cluster: CLUSTER, tasks: taskArns }))).tasks ?? []);
  return { deployments, images, tasks };
}

/** The image the stack runs now, for a probe-only run. */
async function stackImage() {
  const cfn = new CloudFormationClient({});
  const { Stacks = [] } = await cfn.send(new DescribeStacksCommand({ StackName: STACK }));
  const image = Stacks[0]?.Parameters?.find((p) => p.ParameterKey === 'ImageUri')?.ParameterValue;
  if (!image) throw new Error(`stack ${STACK} names no ImageUri`);
  return image;
}

/**
 * Poll until the listener passes, fails, or the time runs out, and return
 * the exit code: 0 keeps the image, 1 rolls it back. A read that fails says
 * nothing about the image, so the probe waits through it rather than
 * failing a good image on a throttled call.
 *
 * @param {{
 *   observe: () => Promise<Observed>,
 *   expected: string,
 *   timeoutMs: number,
 *   now?: () => number,
 *   sleep?: (ms: number) => Promise<unknown>,
 *   log?: (line: string) => void,
 * }} io
 * @returns {Promise<0 | 1>}
 */
export async function probe({
  observe,
  expected,
  timeoutMs,
  now = Date.now,
  sleep = (ms) => new Promise((r) => setTimeout(r, ms)),
  log = console.log,
}) {
  const deadline = now() + timeoutMs;
  let last = '';
  for (;;) {
    /** @type {Judgment} */
    let judgment;
    try {
      judgment = judge(await observe(), expected);
    } catch (err) {
      judgment = { verdict: 'wait', reason: `could not read the service: ${err instanceof Error ? err.message : String(err)}` };
    }
    const { verdict, reason } = judgment;
    if (reason !== last) log(`probe: ${reason}`);
    last = reason;
    if (verdict === 'pass') return 0;
    if (verdict === 'fail') {
      log(`::error::probe failed: ${reason}`);
      return 1;
    }
    if (now() > deadline) {
      log(`::error::probe timed out after ${timeoutMs / 1000}s: ${reason}`);
      return 1;
    }
    await sleep(15_000);
  }
}

async function main() {
  const ecs = new ECSClient({});
  return probe({
    observe: () => observe(ecs),
    expected: process.env.EXPECTED_IMAGE_URI || (await stackImage()),
    timeoutMs: Number(process.env.PROBE_TIMEOUT_SECONDS || 600) * 1000,
  });
}

if (isCliEntry(import.meta.url, process.argv[1])) {
  main().then(
    (code) => process.exit(code),
    (err) => {
      console.error(`::error::probe could not read the service: ${err instanceof Error ? err.message : String(err)}`);
      process.exit(1);
    },
  );
}
