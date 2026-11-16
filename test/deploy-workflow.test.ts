/**
 * The deploy (docs/constraints/deploy.md): a green gate on a push to main
 * applies the stacks and probes the listener, and a red probe rolls the
 * stack back to the image it ran before. Modelled on gh-governor's deploy.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { REPO_ROOT } from './helpers/cfn.ts';
import { loadWorkflow, stepById, stepsOf } from './helpers/workflow.ts';

const { workflow, codeOnly } = loadWorkflow('.github/workflows/deploy.yml');
const steps = stepsOf(workflow, 'deploy');
const planSteps = stepsOf(workflow, 'runner-plan');
const buildSteps = stepsOf(workflow, 'runner-build');
const indexSteps = stepsOf(workflow, 'runner-image');
const indexOf = (id: string) => steps.indexOf(stepById(steps, id));

describe('fitness: only main deploys, one deploy at a time, after a green gate (deploy.md)', () => {
  test('on a push to main and by hand, never on a pull request', () => {
    assert.deepEqual(workflow.on?.push?.branches, ['main']);
    assert.ok('workflow_dispatch' in (workflow.on ?? {}));
    assert.equal(workflow.on?.pull_request, undefined);
  });

  test('after a green gate, and only once the deploy role\'s secret is set', () => {
    assert.equal(workflow.jobs?.gate?.uses, './.github/workflows/gate.yml');
    assert.deepEqual(workflow.jobs?.['runner-plan']?.needs, ['gate', 'role']);
    assert.equal(workflow.jobs?.['runner-plan']?.if, "${{ needs.role.outputs.set == 'true' && !inputs.probe_only }}");
    // Each later runner job waits on the plan, so a skipped plan skips them.
    assert.deepEqual(workflow.jobs?.['runner-build']?.needs, ['runner-plan']);
    assert.equal(workflow.jobs?.['runner-build']?.if, undefined);
    assert.deepEqual(workflow.jobs?.['runner-image']?.needs, ['runner-plan', 'runner-build']);
    assert.equal(workflow.jobs?.['runner-image']?.if, undefined);
    assert.deepEqual(workflow.jobs?.deploy?.needs, ['gate', 'role', 'runner-image']);
    // A probe-only run skips the runner's image, and must still probe; any
    // other run needs the runner's image built.
    assert.equal(
      workflow.jobs?.deploy?.if,
      "${{ !cancelled() && needs.gate.result == 'success' && needs.role.outputs.set == 'true' && (inputs.probe_only || needs.runner-image.result == 'success') }}",
    );
  });

  // GitHub keeps one pending run per concurrency group and cancels the older
  // one when another queues. A newer push carries the older commit, so that
  // loss is harmless; a probe-only run must not take a push's place.
  test('one at a time: a running deploy is never cancelled, and a probe-only run queues apart', () => {
    assert.deepEqual(workflow.concurrency, {
      group: "${{ inputs.probe_only && 'deploy-probe' || 'deploy' }}",
      'cancel-in-progress': false,
    });
  });

  test('inside the deploy role\'s one-hour session, with room for an apply, a rollback and two probes', () => {
    const minutes = workflow.jobs?.deploy?.['timeout-minutes'];
    assert.ok(minutes <= 55 && minutes >= 45, `timeout-minutes is ${minutes}`);
  });
});

describe('fitness: the deploy applies the bootstrap stack, then the main stack, then probes (deploy.md)', () => {
  test('applies the bootstrap stack before the main stack', () => {
    assert.ok(indexOf('bootstrap') < indexOf('apply'));
    assert.match(stepById(steps, 'bootstrap').run ?? '', /--stack-name paddock-bootstrap/);
    assert.match(stepById(steps, 'apply').run ?? '', /--stack-name paddock\b/);
  });

  test('clears a stalled rollback before each apply', () => {
    const guards = steps.filter((s) => /cfn-continue-stalled-rollback\.mjs/.test(s.run ?? ''));
    assert.deepEqual(guards.map((s) => s.run?.trim().split(' ').pop()), ['paddock-bootstrap', 'paddock']);
    assert.ok(steps.indexOf(guards[0]!) < indexOf('bootstrap'));
    assert.ok(steps.indexOf(guards[1]!) < indexOf('apply'));
  });

  test('reads the image the stack runs before applying, applies the image it built, and probes for that image', () => {
    assert.ok(indexOf('previous') < indexOf('apply'));
    assert.ok(indexOf('apply') < indexOf('probe'));
    assert.match(stepById(steps, 'previous').run ?? '', /--stack-name paddock\b/);
    assert.equal(stepById(steps, 'apply').env?.IMAGE_URI, '${{ steps.image.outputs.image_uri }}');
    assert.match(stepById(steps, 'apply').run ?? '', /"ImageUri=\$\{IMAGE_URI\}"/);
    assert.equal(stepById(steps, 'apply').env?.RUNNER_IMAGE_URI, '${{ steps.runner.outputs.image_uri }}');
    assert.match(stepById(steps, 'apply').run ?? '', /"RunnerImageUri=\$\{RUNNER_IMAGE_URI\}"/);
    assert.match(stepById(steps, 'previous').run ?? '', /read_parameter ImageUri\)[\s\S]*read_parameter RunnerImageUri\)/);
    assert.ok(indexOf('runner') < indexOf('apply'));
    assert.equal(stepById(steps, 'probe').env?.EXPECTED_IMAGE_URI, '${{ steps.image.outputs.image_uri }}');
  });

  // ECS creates its service-linked role on an account's first cluster, and
  // a cluster with capacity providers can fail to attach them while it does.
  // The failed first create leaves a stack only a delete clears.
  test('makes sure ECS\'s service-linked role exists before the first apply can need it', () => {
    const run = stepById(steps, 'ecs-role').run ?? '';
    assert.match(run, /aws iam get-role --role-name AWSServiceRoleForECS/);
    assert.match(run, /aws iam create-service-linked-role --aws-service-name ecs\.amazonaws\.com/);
    assert.equal(stepById(steps, 'ecs-role').if, '${{ !inputs.probe_only }}');
    assert.ok(indexOf('ecs-role') < indexOf('apply'));
  });

  test('a probe-only run applies nothing', () => {
    const guards = steps.filter((s) => /cfn-continue-stalled-rollback\.mjs/.test(s.run ?? ''));
    for (const step of [...['bootstrap', 'plan', 'image', 'runner', 'network', 'ecs-role', 'previous', 'apply'].map((id) => stepById(steps, id)), ...guards]) {
      assert.equal(step.if, '${{ !inputs.probe_only }}', `${step.id ?? step.name} runs on a probe-only run`);
    }
    assert.equal(stepById(steps, 'probe').if, undefined);
  });
});

describe('fitness: a red probe rolls the stack back to both images it ran before, and a failed rollback pages (deploy.md)', () => {
  const rollback = stepById(steps, 'rollback');
  const reprobe = stepById(steps, 'reprobe');
  const page = stepById(steps, 'page');

  test('rolls back only after a red probe that changed either image, and never on a probe-only run', () => {
    assert.equal(
      rollback.if,
      "${{ failure() && !inputs.probe_only && steps.probe.outcome == 'failure' && steps.previous.outputs.image_uri != '' && (steps.previous.outputs.image_uri != steps.image.outputs.image_uri || (steps.previous.outputs.runner_image_uri != '' && steps.previous.outputs.runner_image_uri != steps.runner.outputs.image_uri)) }}",
    );
  });

  test('re-applies the same template with both images before, changing nothing else', () => {
    const run = rollback.run ?? '';
    assert.match(run, /--stack-name paddock\b/);
    assert.match(run, /--template-file infra\/paddock\.cfn\.yml/);
    assert.deepEqual([...run.matchAll(/"(\w+)=/g)].map((m) => m[1]), ['ImageUri', 'RunnerImageUri']);
    assert.equal(rollback.env?.PREVIOUS_IMAGE_URI, '${{ steps.previous.outputs.image_uri }}');
    assert.equal(rollback.env?.PREVIOUS_RUNNER_IMAGE_URI, '${{ steps.previous.outputs.runner_image_uri }}');
  });

  // A stack from before the runner's image has no runner image to return
  // to; the rollback then keeps the one just built and rolls the listener
  // back alone.
  test('keeps the new runner\'s image when the stack ran none before', () => {
    const run = rollback.run ?? '';
    assert.match(run, /\$\{PREVIOUS_RUNNER_IMAGE_URI:-\$RUNNER_IMAGE_URI\}/);
    assert.equal(rollback.env?.RUNNER_IMAGE_URI, '${{ steps.runner.outputs.image_uri }}');
  });

  test('probes the image it rolled back to', () => {
    assert.equal(reprobe.if, "${{ failure() && steps.rollback.outcome == 'success' }}");
    assert.equal(reprobe.env?.EXPECTED_IMAGE_URI, '${{ steps.previous.outputs.image_uri }}');
    assert.match(reprobe.run ?? '', /node scripts\/probe\.mjs/);
  });

  // A job that reaches its timeout is cancelled, not failed, and the
  // listener's state is then unknown.
  test('pages the operator when the rollback does not bring the listener back, or runs out of time', () => {
    assert.equal(
      page.if,
      "${{ (failure() || cancelled()) && (steps.rollback.outcome == 'failure' || steps.rollback.outcome == 'cancelled' || steps.reprobe.outcome == 'failure' || steps.reprobe.outcome == 'cancelled') }}",
    );
    assert.match(page.run ?? '', /--stack-name paddock --logical-resource-id AlertsTopic/);
    assert.match(page.run ?? '', /aws sns publish/);
  });
});

// A failed apply's reason sits in the stack's events, which only someone
// with AWS credentials can read. The step prints them to the log, after the
// credentials step, which masks the account id an ARN in a reason may carry.
describe('a failed apply prints the stack\'s failed events, then fails', () => {
  const applies: Array<[string, string]> = [['bootstrap', 'paddock-bootstrap'], ['apply', 'paddock'], ['rollback', 'paddock']];

  // Runs the step as Actions runs it, with `aws` and `node` replaced by
  // stubs that record their arguments, and `aws` exiting `awsStatus`.
  const runStep = (id: string, awsStatus: number) => {
    const dir = mkdtempSync(join(tmpdir(), 'deploy-apply-'));
    try {
      const calls = join(dir, 'calls');
      writeFileSync(join(dir, 'aws'), `#!/bin/sh\necho "aws $*" >> "$CALLS"\nexit ${awsStatus}\n`, { mode: 0o755 });
      writeFileSync(join(dir, 'node'), '#!/bin/sh\necho "node $*" >> "$CALLS"\n', { mode: 0o755 });
      const started = Math.floor(Date.now() / 1000) * 1000;
      const result = spawnSync('bash', ['-e', '-c', stepById(steps, id).run ?? ''], {
        env: { PATH: `${dir}:${process.env.PATH}`, CALLS: calls },
        stdio: 'pipe',
      });
      const ended = Date.now();
      const lines = existsSync(calls) ? readFileSync(calls, 'utf-8').trim().split('\n') : [];
      return { status: result.status, lines, started, ended };
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  };

  for (const [id, stack] of applies) {
    test(`${id}: a green apply prints nothing more`, () => {
      const { status, lines } = runStep(id, 0);
      assert.equal(status, 0);
      assert.equal(lines.length, 1, lines.join('\n'));
      assert.match(lines[0]!, new RegExp(`^aws cloudformation deploy --stack-name ${stack} `));
    });

    test(`${id}: a failed apply prints ${stack}'s failed events since it started, then fails with the apply's code`, () => {
      const { status, lines, started, ended } = runStep(id, 255);
      assert.equal(status, 255);
      assert.equal(lines.length, 2, lines.join('\n'));
      assert.match(lines[0]!, new RegExp(`^aws cloudformation deploy --stack-name ${stack} `));
      const printed = new RegExp(`^node scripts/cfn-failed-events\\.mjs ${stack} (\\S+)$`).exec(lines[1]!);
      assert.ok(printed, lines[1]);
      const since = Date.parse(printed[1]!);
      assert.ok(since >= started && since <= ended, `the apply started at ${printed[1]}`);
    });
  }

  test('each runs after the credentials step and the scripts\' install', () => {
    const credentials = steps.findIndex((s) => s.uses?.startsWith('aws-actions/configure-aws-credentials@'));
    const install = steps.findIndex((s) => /^npm ci\b/.test(s.run ?? ''));
    assert.ok(credentials >= 0 && install > credentials);
    for (const [id] of applies) assert.ok(indexOf(id) > install, `${id} runs before the install`);
  });
});

describe('fitness: the image tag hashes exactly what the image is built from (deploy.md)', () => {
  // The tag hashes what the image is built from, so an unchanged listener
  // reuses its image and a changed one cannot. A file the Dockerfile copies
  // but the hash leaves out would ship under an old tag.
  test('hashes exactly the files the listener\'s Dockerfile copies, and the Dockerfile', () => {
    const dockerfile = readFileSync(resolve(REPO_ROOT, 'cmd/listener/Dockerfile'), 'utf-8');
    assert.doesNotMatch(dockerfile, /^ADD /m, 'an ADD would bring in a file the hash cannot see');
    const copied = [...dockerfile.matchAll(/^COPY (?!--from)(.+?) \S+$/gm)]
      .flatMap((m) => m[1]!.trim().split(/\s+/))
      .map((p) => p.replace(/\/$/, ''));
    const hashed = /git ls-tree -r HEAD -- (.+?) \|/.exec(stepById(steps, 'plan').run?.replace(/\\\n/g, ' ').replace(/\s+/g, ' ') ?? '')?.[1]?.split(' ') ?? [];
    assert.deepEqual([...hashed].sort(), [...copied, 'cmd/listener/Dockerfile'].sort());
  });

  test('builds the listener\'s image from its Dockerfile', () => {
    const image = stepById(steps, 'image');
    assert.equal(image.uses, './.github/actions/image');
    assert.equal(image.with?.dockerfile, 'cmd/listener/Dockerfile');
    assert.equal(image.with?.image_tag, '${{ steps.plan.outputs.image_tag }}');
  });
});

describe('fitness: the runner\'s image is built for x64 and arm64, each on a runner of that architecture, and handed on as one index (deploy.md)', () => {
  test('a job per architecture, each on a GitHub-hosted runner of that architecture, never under emulation', () => {
    const job = workflow.jobs?.['runner-build'];
    assert.equal(job?.['runs-on'], '${{ matrix.runs-on }}');
    assert.deepEqual(job?.strategy?.matrix, {
      include: [
        { arch: 'x64', 'runs-on': 'ubuntu-24.04', platform: 'linux/amd64' },
        { arch: 'arm64', 'runs-on': 'ubuntu-24.04-arm', platform: 'linux/arm64' },
      ],
    });
    const image = stepById(buildSteps, 'image');
    assert.equal(image.uses, './.github/actions/image');
    assert.equal(image.with?.dockerfile, 'runner/Dockerfile');
    assert.equal(image.with?.repo_name, 'paddock-runner');
    assert.equal(image.with?.platform, '${{ matrix.platform }}');
    assert.equal(image.with?.image_tag, '${{ needs.runner-plan.outputs.image_tag }}-${{ matrix.arch }}');
    assert.equal(image.with?.cache_scope, 'runner-${{ matrix.arch }}');
    assert.equal(image.with?.compression, 'gzip');
  });

  test('its tag hashes runner/, which holds the Dockerfile and nothing it copies from elsewhere', () => {
    const dockerfile = readFileSync(resolve(REPO_ROOT, 'runner/Dockerfile'), 'utf-8');
    assert.doesNotMatch(dockerfile, /^(ADD|COPY) (?!--from)/m, 'the runner\'s image copies nothing from the repo');
    const run = stepById(planSteps, 'plan').run?.replace(/\\\n/g, ' ').replace(/\s+/g, ' ') ?? '';
    assert.match(run, /git ls-tree -r HEAD -- runner \|/);
    assert.deepEqual(workflow.jobs?.['runner-plan']?.outputs, { image_tag: '${{ steps.plan.outputs.image_tag }}' });
  });

  // A job output that holds a masked string is dropped, and the image URI
  // carries the account id, which the credentials step masks. The job hands
  // on the digest, and the deploy job builds the URI from its own registry.
  test('hands the deploy job the index\'s digest alone, which it joins to its own registry', () => {
    assert.deepEqual(workflow.jobs?.['runner-image']?.outputs, { digest: '${{ steps.index.outputs.digest }}' });
    assert.equal(stepById(indexSteps, 'index').env?.TAG, '${{ needs.runner-plan.outputs.image_tag }}');
    const runner = stepById(steps, 'runner');
    assert.equal(runner.env?.DIGEST, '${{ needs.runner-image.outputs.digest }}');
    assert.match(runner.run ?? '', /paddock-runner@\$\{DIGEST\}/);
  });

  // Runs the index step as Actions runs it, with `aws`, `docker`, `skopeo`
  // and `soci` replaced by stubs. `tags` maps a tag in the registry to its
  // digest; a tag not in it is not found, unless `denied`, when every read
  // is refused. `pushes` maps a tag to the digest it names once docker or
  // skopeo pushes to it. Every image found is of the media type `media`, an
  // OCI index unless set, and its manifest is `manifest`, an index of both
  // architectures with a SOCI index for each unless set.
  type IndexOpts = { denied?: boolean; pushes?: Record<string, string>; media?: string; manifest?: unknown };
  const runIndex = (tags: Record<string, string>, opts: IndexOpts = {}) => {
    const dir = mkdtempSync(join(tmpdir(), 'deploy-index-'));
    try {
      const calls = join(dir, 'calls');
      const output = join(dir, 'output');
      const pushed = join(dir, 'pushed');
      const bin = join(dir, 'bin');
      mkdirSync(pushed);
      mkdirSync(bin);
      const table = Object.entries(tags).map(([tag, digest]) => `"${tag}") digest="${digest}" ;;`).join('\n');
      const pushTable = Object.entries(opts.pushes ?? {}).map(([tag, digest]) => `"${tag}") digest="${digest}" ;;`).join('\n');
      writeFileSync(join(dir, 'manifest.json'), JSON.stringify(opts.manifest ?? sociIndex()));
      writeFileSync(join(bin, 'aws'), `#!/bin/bash
echo "aws $*" >> "$CALLS"
case "$1 $2" in
  "ecr describe-images" | "ecr batch-get-image")
    ${opts.denied ? 'echo "An error occurred (AccessDeniedException)" >&2; exit 254' : ''}
    tag=""; media=""
    for a in "$@"; do case "$a" in imageTag=*) tag="\${a#imageTag=}" ;; *imageManifestMediaType*) media=1 ;; esac; done
    digest=""
    if [ -f "$PUSHED/$tag" ]; then
      case "$tag" in
${pushTable}
      esac
    fi
    if [ -z "$digest" ]; then
      case "$tag" in
${table}
      esac
    fi
    if [ -n "$digest" ]; then
      if [ "$2" = batch-get-image ]; then cat "$MANIFEST"
      elif [ -n "$media" ]; then echo "${opts.media ?? 'application/vnd.oci.image.index.v1+json'}"; else echo "$digest"; fi
      exit 0
    fi
    echo "An error occurred (ImageNotFoundException)" >&2; exit 254 ;;
  "sts get-caller-identity") echo the-account ;;
  "ecr get-login-password") echo token ;;
esac
`, { mode: 0o755 });
      writeFileSync(join(bin, 'docker'), `#!/bin/bash
echo "docker $*" >> "$CALLS"
if [ "$1 $2 $3" = "buildx imagetools create" ]; then t="$5"; touch "$PUSHED/\${t##*:}"; fi
cat > /dev/null
`, { mode: 0o755 });
      writeFileSync(join(bin, 'skopeo'), `#!/bin/bash
echo "skopeo $*" >> "$CALLS"
[ "$1" = login ] && { cat > /dev/null; exit 0; }
dest="\${@: -1}"
case "$dest" in
  oci:*) mkdir -p "\${dest#oci:}" ;;
  docker://*) touch "$PUSHED/\${dest##*:}" ;;
esac
`, { mode: 0o755 });
      writeFileSync(join(bin, 'soci'), `#!/bin/bash
echo "soci $*" >> "$CALLS"
mkdir -p "\${@: -1}"
`, { mode: 0o755 });
      const result = spawnSync('bash', ['-e', '-c', stepById(indexSteps, 'index').run ?? ''], {
        env: { PATH: `${bin}:${process.env.PATH}`, CALLS: calls, PUSHED: pushed, MANIFEST: join(dir, 'manifest.json'), GITHUB_OUTPUT: output, RUNNER_TEMP: dir, TAG: 'abc' },
        stdio: 'pipe',
        input: '',
      });
      const lines = existsSync(calls) ? readFileSync(calls, 'utf-8').trim().split('\n').map((l) => l.replaceAll(dir, '$RUNNER_TEMP')) : [];
      const out = existsSync(output) ? readFileSync(output, 'utf-8') : '';
      return { status: result.status, lines, out, stderr: result.stderr.toString() };
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  };
  const d = (c: string) => `sha256:${c.repeat(64)}`;
  const REGISTRY = 'the-account.dkr.ecr.us-east-1.amazonaws.com/paddock-runner';
  const SOCI = 'application/vnd.amazon.soci.index.v2+json';
  const image = (arch: string, c: string) => ({ mediaType: 'application/vnd.oci.image.manifest.v1+json', digest: d(c), size: 1, platform: { architecture: arch, os: 'linux' } });
  const soci = (arch: string, c: string) => ({ ...image(arch, c), artifactType: SOCI });
  const unplaced = (c: string) => ({ mediaType: 'application/vnd.oci.image.manifest.v1+json', digest: d(c), size: 1, artifactType: SOCI });
  const sociIndex = (manifests: object[] = [image('amd64', 'a'), image('arm64', 'b'), soci('amd64', 'c'), soci('arm64', 'e')]) => ({
    schemaVersion: 2,
    mediaType: 'application/vnd.oci.image.index.v1+json',
    manifests,
  });
  const built = { 'abc-x64': d('1'), 'abc-arm64': d('2') };
  const pushes = { 'abc-gzip': d('3'), abc: d('5') };

  test('joins both architectures\' gzip images under the plan\'s tag with -gzip after it', () => {
    const { status, lines, stderr } = runIndex(built, { pushes });
    assert.equal(status, 0, stderr);
    const create = lines.find((l) => l.startsWith('docker buildx imagetools create'));
    assert.equal(create, `docker buildx imagetools create --tag ${REGISTRY}:abc-gzip ${REGISTRY}@${d('1')} ${REGISTRY}@${d('2')}`);
  });

  // soci convert --standalone reads and writes an OCI image layout on disk,
  // with no containerd; skopeo moves the index to and from the registry.
  test('converts the joined index with a SOCI index for each architecture, pushes it at the plan\'s tag, and hands on its digest', () => {
    const { status, lines, out, stderr } = runIndex(built, { pushes });
    assert.equal(status, 0, stderr);
    const tools = lines.filter((l) => /^(skopeo|soci) /.test(l));
    assert.deepEqual(tools, [
      'skopeo login --authfile $RUNNER_TEMP/auth.json --username AWS --password-stdin the-account.dkr.ecr.us-east-1.amazonaws.com',
      `skopeo copy --all --preserve-digests --authfile $RUNNER_TEMP/auth.json docker://${REGISTRY}@${d('3')} oci:$RUNNER_TEMP/gzip`,
      'soci convert --standalone --all-platforms --format oci-dir $RUNNER_TEMP/gzip $RUNNER_TEMP/soci',
      `skopeo copy --all --format oci --authfile $RUNNER_TEMP/auth.json oci:$RUNNER_TEMP/soci docker://${REGISTRY}:abc`,
    ]);
    assert.equal(out, `digest=${d('5')}\n`);
  });

  test('converts a joined index an earlier run left, and joins nothing', () => {
    const { status, lines, out, stderr } = runIndex({ ...built, 'abc-gzip': d('3') }, { pushes });
    assert.equal(status, 0, stderr);
    assert.ok(!lines.some((l) => l.startsWith('docker buildx')), lines.join('\n'));
    assert.ok(lines.some((l) => l.startsWith('soci convert')), lines.join('\n'));
    assert.equal(out, `digest=${d('5')}\n`);
  });

  test('reuses an index already under the tag, and joins and converts nothing', () => {
    const { status, lines, out, stderr } = runIndex({ abc: d('4'), ...built });
    assert.equal(status, 0, stderr);
    assert.ok(!lines.some((l) => /^(docker|skopeo|soci) /.test(l)), lines.join('\n'));
    assert.equal(out, `digest=${d('4')}\n`);
  });

  // Fargate starts a task from an index without a SOCI index all the same,
  // pulling the whole image, so nothing downstream would notice one lost.
  test('refuses, handing on nothing, an index without a SOCI index for each architecture', () => {
    const cases = [
      sociIndex([image('amd64', 'a'), image('arm64', 'b')]),
      sociIndex([image('amd64', 'a'), image('arm64', 'b'), soci('amd64', 'c')]),
      sociIndex([image('amd64', 'a'), soci('amd64', 'c'), soci('arm64', 'e')]),
      sociIndex([image('amd64', 'a'), image('arm64', 'b'), { ...soci('arm64', 'e'), artifactType: 'application/vnd.amazon.soci.index.v1+json' }, soci('amd64', 'c')]),
      sociIndex([image('amd64', 'a'), image('arm64', 'b'), soci('amd64', 'c'), soci('amd64', 'e')]),
      sociIndex([image('amd64', 'a'), image('arm64', 'b'), unplaced('c')]),
    ];
    for (const manifest of cases) {
      const { status, out } = runIndex({ abc: d('4') }, { manifest });
      assert.notEqual(status, 0, JSON.stringify(manifest));
      assert.equal(out, '', JSON.stringify(manifest));
    }
  });

  // Nothing published says whether soci convert names a platform on each
  // SOCI index's entry. Where every entry names one, each architecture needs
  // its own; where none does, one per image is the most the index shows.
  test('takes SOCI indexes that name no platform, one for each image', () => {
    const manifest = sociIndex([image('amd64', 'a'), image('arm64', 'b'), unplaced('c'), unplaced('e')]);
    const { status, out, stderr } = runIndex({ abc: d('4') }, { manifest });
    assert.equal(status, 0, stderr);
    assert.equal(out, `digest=${d('4')}\n`);
  });

  // An earlier deploy pushed a single x64 image at a bare tag. A tree of
  // runner/ that hashes to one of those tags must not hand on that image as
  // if it were the index: an arm64 pool's tasks would pull an x64 image.
  test('refuses, handing on nothing, a tag that holds a single image rather than an index', () => {
    for (const media of ['application/vnd.docker.distribution.manifest.v2+json', 'application/vnd.oci.image.manifest.v1+json']) {
      const { status, lines, out } = runIndex({ abc: d('4'), ...built }, { media });
      assert.notEqual(status, 0, media);
      assert.ok(!lines.some((l) => l.startsWith('docker buildx')), lines.join('\n'));
      assert.equal(out, '', media);
    }
    const docker = runIndex({ abc: d('4') }, { media: 'application/vnd.docker.distribution.manifest.list.v2+json' });
    assert.equal(docker.out, `digest=${d('4')}\n`, docker.stderr);
  });

  test('fails, handing on nothing, when an architecture\'s image is missing or the registry refuses a read', () => {
    for (const [tags, opts] of [[{ 'abc-x64': d('1') }, { pushes }], [{ 'abc-arm64': d('2') }, { pushes }], [built, { denied: true, pushes }]] as const) {
      const { status, lines, out } = runIndex(tags, opts);
      assert.notEqual(status, 0, JSON.stringify(tags));
      assert.ok(!lines.some((l) => /^(docker buildx|skopeo copy|soci) /.test(l)), lines.join('\n'));
      assert.equal(out, '');
    }
  });
});

describe('fitness: the deploy runs in us-east-1 alone (region.md)', () => {
  test('every AWS step names us-east-1, and no repository variable can move it', () => {
    assert.deepEqual([...codeOnly.matchAll(/\bvars\.([A-Za-z_]+)/g)].map((m) => m[1]), []);
    const regions = [...codeOnly.matchAll(/(?:aws-region|region): (\S+)/g)].map((m) => m[1]);
    assert.ok(regions.length >= 3, regions.join(', '));
    for (const region of regions) assert.equal(region, 'us-east-1');
  });
});

// Governor learns of a deploy from the deploy workflow alone
// (.github/governor.json turns deployMessages on). The job whose success
// means the change is live ends its name with the marker and names the
// commit it shipped, and the file says which merged pull requests hear of it.
describe('what governor reads of a deploy', () => {
  const name: string = workflow.jobs?.deploy?.name ?? '';

  test('the deploy job\'s name ends with the commit and the target, on a push or a run by hand', () => {
    assert.match(name, /\|\| format\('[^']* at \{0\} \u00b7 deployed: paddock', github\.sha\) \}\}$/);
  });

  test('a probe-only run deploys nothing, so its name carries no marker', () => {
    const probeOnly = /inputs\.probe_only && '([^']*)'/.exec(name);
    assert.ok(probeOnly, name);
    assert.doesNotMatch(probeOnly[1]!, /deployed:/);
  });

  test('no other job is marked: a rollback is a step of the deploy job, which then ends red', () => {
    for (const [id, job] of Object.entries(workflow.jobs ?? {}) as [string, { name?: string }][]) {
      if (id !== 'deploy') assert.doesNotMatch(job.name ?? id, /deployed:/, id);
    }
  });

  test('every pull request a deploy shipped hears of it', () => {
    assert.deepEqual(workflow.env, { TICKET_LABEL_PATHS: 'all' });
  });
});
