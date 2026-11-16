/**
 * The gate (CLAUDE.md, The gate): every pull request, from a branch here or
 * from a fork, runs `npm run gate` and builds the listener's image, and no
 * workflow here runs on the runners this repo deploys
 * (docs/constraints/public-repo.md).
 *
 * gate.yml runs on pull_request, not on a branch push. Each commit on a pull
 * request then gets one run, and its checks are `gate` and `image` whether
 * the branch is here or on a fork, so a required `gate` check meets both. A
 * branch push would add a second run beside the pull request's, and a fork's
 * pull request has no push run here at all. main is gated by deploy.yml,
 * which calls this before it applies.
 *
 * A pull request run checks out the merge of its head into its base branch,
 * not the head, so nothing the gate runs may depend on the head's own
 * history. A pull request that conflicts with its base gets no run until
 * the conflict is resolved.
 */

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { REPO_ROOT } from './helpers/cfn.ts';
import { joinContinuations, loadWorkflow, stepById, stepsOf } from './helpers/workflow.ts';

const { raw, workflow } = loadWorkflow('.github/workflows/gate.yml');
const GATE_CHECKS = ['gate', 'image', 'runner-image', 'soci'];

const workflowFiles = readdirSync(resolve(REPO_ROOT, '.github/workflows')).filter((f) => /\.ya?ml$/.test(f));
const workflows = workflowFiles.map((file) => ({ file, ...loadWorkflow(`.github/workflows/${file}`) }));
const triggers = (w: Record<string, any>): string[] =>
  typeof w.on === 'string' ? [w.on] : Array.isArray(w.on) ? w.on : Object.keys(w.on ?? {});

describe('the gate', () => {
  test('runs on every pull request, and when another workflow calls it, never on a branch push', () => {
    assert.deepEqual(triggers(workflow).sort(), ['pull_request', 'workflow_call']);
    // No branch or path filter: every pull request gets its gate.
    assert.equal(workflow.on?.pull_request ?? null, null);
    assert.deepEqual(workflow.permissions, { contents: 'read' });
  });

  // pull_request_target runs a fork's code with this repo's write token and
  // secrets. gate.yml's own triggers are pinned above; this holds every
  // other workflow, including one that would call the gate.
  test('no workflow runs on pull_request_target', () => {
    for (const { file, workflow: w } of workflows) {
      assert.ok(!triggers(w).includes('pull_request_target'), `${file} runs on pull_request_target`);
    }
  });

  test('its jobs run their steps whenever it runs: neither carries an `if` that could skip it', () => {
    for (const job of GATE_CHECKS) assert.equal(workflow.jobs?.[job]?.if, undefined, job);
  });

  // A skipped job finishes at once and reports success, and a required check
  // passes on it. A second `gate` or `image` on a pull request, from another
  // workflow, could be skipped and read as green before gate.yml's finished.
  test('no other workflow a pull request runs has a job whose check is named `gate` or `image`', () => {
    for (const { file, workflow: w } of workflows) {
      if (file === 'gate.yml') continue;
      if (!triggers(w).some((t) => t.startsWith('pull_request'))) continue;
      for (const [id, job] of Object.entries(w.jobs ?? {}) as [string, { name?: string }][]) {
        assert.ok(!GATE_CHECKS.includes(job.name ?? id), `${file} job ${id} reports a check named ${job.name ?? id}`);
      }
    }
  });

  test('runs the same `npm run gate` a contributor runs, with Go installed', () => {
    const steps = stepsOf(workflow, 'gate');
    assert.ok(steps.some((s) => s.run === 'npm run gate'));
    assert.ok(steps.some((s) => s.uses?.startsWith('actions/setup-go@')));
  });

  // The listener is built with the Dockerfile's golang tag, and Dependabot
  // moves that tag alone. A second copy of the release in the gate would let
  // `go vet` and `go test` run on one release while the image ships another.
  describe('runs Go on the release the listener\'s image builds with', () => {
    const steps = stepsOf(workflow, 'gate');
    const setupGo = steps.find((s) => s.uses?.startsWith('actions/setup-go@'));
    const ref = /^\$\{\{\s*steps\.([\w-]+)\.outputs\.([\w-]+)\s*\}\}$/.exec(String(setupGo?.with?.['go-version'] ?? ''));

    test('setup-go takes its release from an earlier step\'s output, not a literal', () => {
      assert.ok(ref, `go-version is ${JSON.stringify(setupGo?.with?.['go-version'])}`);
      assert.equal(setupGo?.with?.['go-version-file'], undefined);
      const reader = steps.findIndex((s) => s.id === ref[1]);
      assert.ok(reader >= 0 && reader < steps.indexOf(setupGo!), `step ${ref[1]} runs before setup-go`);
    });

    // Runs the reader step, as Actions runs it, against `dockerfile` placed at
    // cmd/listener/Dockerfile in a scratch directory, and returns the release
    // it outputs. Throws when the step exits non-zero.
    const readRelease = (dockerfile: string): string | undefined => {
      assert.ok(ref);
      const dir = mkdtempSync(join(tmpdir(), 'gate-go-'));
      try {
        mkdirSync(join(dir, 'cmd/listener'), { recursive: true });
        writeFileSync(join(dir, 'cmd/listener/Dockerfile'), dockerfile);
        const output = join(dir, 'output');
        execFileSync('bash', ['-eo', 'pipefail', '-c', stepById(steps, ref[1] ?? '').run ?? ''], {
          cwd: dir,
          env: { PATH: process.env.PATH, GITHUB_OUTPUT: output },
          stdio: 'pipe',
        });
        const outputs = Object.fromEntries(
          readFileSync(output, 'utf-8').trim().split('\n').map((line) => {
            const at = line.indexOf('=');
            return [line.slice(0, at), line.slice(at + 1)];
          }),
        );
        return outputs[ref[2] ?? ''];
      } finally {
        rmSync(dir, { recursive: true, force: true });
      }
    };

    test('that step reads the release from cmd/listener/Dockerfile\'s golang FROM line', () => {
      const dockerfile = readFileSync(resolve(REPO_ROOT, 'cmd/listener/Dockerfile'), 'utf-8');
      const tag = /^FROM\s.*golang:(\d+\.\d+(?:\.\d+)?)/m.exec(dockerfile)?.[1];
      assert.ok(tag, 'the Dockerfile builds FROM a golang:<release> image');
      assert.equal(readRelease(dockerfile), tag);
    });

    test('that step reads the release whatever the FROM line carries around the tag', () => {
      const cases: [string, string][] = [
        ['FROM public.ecr.aws/docker/library/golang:1.27-bookworm AS build', '1.27'],
        ['FROM golang:1.27.3-alpine AS build', '1.27.3'],
        ['FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build', '1.27'],
        ['FROM golang:1.27-bookworm@sha256:0123abcd AS build', '1.27'],
        ['FROM golang:1.27 AS build', '1.27'],
        ['FROM golang:1.27', '1.27'],
      ];
      for (const [from, release] of cases) {
        assert.equal(readRelease(`# a comment\n${from}\nRUN true\nFROM scratch\n`), release, from);
      }
    });

    test('that step fails the gate when no FROM line names a golang release', () => {
      for (const dockerfile of ['FROM scratch\n', 'FROM golang:latest\n', 'FROM golang:1.27rc1-bookworm\n']) {
        assert.throws(() => readRelease(dockerfile), JSON.stringify(dockerfile));
      }
    });

    test('the comment above setup-go says the release comes from the Dockerfile', () => {
      const lines = raw.split('\n');
      const at = lines.findIndex((l) => /uses: actions\/setup-go@/.test(l));
      let i = at - 1;
      while (i >= 0 && /^\s*#/.test(lines[i] ?? '')) i--;
      const comment = lines.slice(i + 1, at).join(' ');
      assert.match(comment, /cmd\/listener\/Dockerfile/);
      assert.match(comment, /Dependabot/);
    });
  });

  // ECS runs the image as its nonroot user and calls the health check inside
  // it. The listener beats only once GitHub has accepted the App key, which
  // the offline gate cannot give it, so the gate proves the entrypoint runs
  // and that the health check stays red until a beat.
  test('builds the listener\'s image, starts it, and its health check stays red with no beat', () => {
    const runs = stepsOf(workflow, 'image').map((s) => s.run ?? '').join('\n');
    assert.match(runs, /docker build --file cmd\/listener\/Dockerfile/);
    assert.match(runs, /-healthcheck/);
    assert.match(runs, /grep -q "no heartbeat"/);
    assert.match(runs, /grep -q '"msg":"listener started"'/);
  });

  // One job per architecture, each on a GitHub runner of that architecture:
  // a build under emulation is slow, and proves less.
  test('fitness: the gate builds the runner\'s image on x64 and on arm64, each natively, and starts each one\'s runner (runner-image.md)', () => {
    const job = workflow.jobs?.['runner-image'];
    assert.equal(job?.['runs-on'], '${{ matrix.runs-on }}');
    assert.deepEqual(job?.strategy?.matrix, {
      include: [
        { arch: 'x64', 'runs-on': 'ubuntu-24.04', platform: 'linux/amd64', machine: 'x86_64' },
        { arch: 'arm64', 'runs-on': 'ubuntu-24.04-arm', platform: 'linux/arm64', machine: 'aarch64' },
      ],
    });
    // One architecture's failure must not cancel the other's answer.
    assert.equal(job?.strategy?.['fail-fast'], false);
    assert.equal(job?.name, 'runner-image (${{ matrix.arch }})');
    const steps = stepsOf(workflow, 'runner-image');
    const runs = steps.map((s) => s.run ?? '').join('\n');
    assert.match(runs, /docker buildx build --platform "\$PLATFORM" --provenance=false --file runner\/Dockerfile /);
    assert.match(runs, /--entrypoint \/home\/runner\/bin\/Runner\.Listener \S+ --version/);
    for (const tool of ['aws --version', 'jq --version', 'zip -v', 'python3 --version']) assert.ok(runs.includes(tool), tool);
    // The image the job built is the architecture it was asked for.
    assert.match(runs, /test "\$\(uname -m\)" = "\$MACHINE"/);
    for (const step of steps.filter((s) => s.run)) {
      if (/\$PLATFORM/.test(step.run ?? '')) assert.equal(step.env?.PLATFORM, '${{ matrix.platform }}');
      if (/\$MACHINE/.test(step.run ?? '')) assert.equal(step.env?.MACHINE, '${{ matrix.machine }}');
    }
  });

  // The entrypoint is written inline in the Dockerfile, so only a real
  // build shows it landed whole and runs: started with no config, it says so.
  test('the gate starts the image\'s own entrypoint, which refuses to run with no config', () => {
    const runs = stepsOf(workflow, 'runner-image').map((s) => s.run ?? '').join('\n');
    assert.match(runs, /docker run --rm paddock-runner:gate >"\$RUNNER_TEMP\/entrypoint\.out" 2>&1/);
    assert.match(runs, /grep -q "the task was given no config" "\$RUNNER_TEMP\/entrypoint\.out"/);
  });
});

// The deploy builds the runner's image through the image action, with buildx
// and an image output that recompresses every layer. The gate builds it with
// the same output options, exported to a local OCI tarball, so a mistake in
// them, or a buildx release that refuses one, fails a pull request rather
// than the deploy on main.
describe('the gate builds the runner\'s image with the deploy\'s buildx output options', () => {
  const steps = stepsOf(workflow, 'runner-image');
  const build = steps.find((s) => /docker buildx build /.test(s.run ?? ''));
  const command = joinContinuations(build?.run ?? '');

  // An exporter's options as a map, less the ones that only say where the
  // image goes: the deploy pushes it, the gate writes a file and loads it.
  const WHERE = ['type', 'push', 'dest', 'name'];
  const options = (output: string): Record<string, string> =>
    Object.fromEntries(
      output
        .split(',')
        .map((pair): [string, string] => {
          const at = pair.indexOf('=');
          return at < 0 ? [pair, ''] : [pair.slice(0, at), pair.slice(at + 1)];
        })
        .filter(([key]) => !WHERE.includes(key)),
    );
  const outputs = [...command.matchAll(/--output[ =]("[^"]*"|\S+)/g)].map((m) => (m[1] ?? '').replace(/^"|"$/g, ''));
  const oci = outputs.find((o) => o.startsWith('type=oci,'));

  const action = loadWorkflow('.github/actions/image/action.yml').workflow;
  const actionBuild = (action.runs?.steps ?? []).find((s: { uses?: string }) => s.uses?.startsWith('docker/build-push-action@'));
  const deploy = loadWorkflow('.github/workflows/deploy.yml').workflow;
  const compression = stepById(stepsOf(deploy, 'runner-build'), 'image').with?.compression;
  const format = /format\('([^']*)', inputs\.compression\)/.exec(String(actionBuild?.with?.outputs ?? ''))?.[1];

  test('it builds with buildx, on a builder set up for the job', () => {
    assert.ok(build, 'a step runs docker buildx build');
    const setup = steps.findIndex((s) => s.uses?.startsWith('docker/setup-buildx-action@'));
    assert.ok(setup >= 0 && setup < steps.indexOf(build!), 'setup-buildx-action runs before the build');
  });

  test('it writes the image to a local OCI tarball and loads it to run, pushing nothing', () => {
    assert.ok(oci, `outputs are ${JSON.stringify(outputs)}`);
    assert.match(oci!, /,dest=\$RUNNER_TEMP\/[\w.-]+\.tar(,|$)/);
    assert.ok(outputs.includes('type=docker,name=paddock-runner:gate'), 'the image is loaded under the tag the later steps run');
    assert.doesNotMatch(command, /push=true|--push\b/);
    assert.doesNotMatch(JSON.stringify(workflow.jobs?.['runner-image']), /secrets\.|aws-actions|id-token/);
  });

  test('its OCI output carries the same options as the deploy\'s image output for the runner', () => {
    assert.ok(format, 'the image action formats its output from the compression');
    assert.equal(typeof compression, 'string');
    const deployed = options(format!.replace('{0}', String(compression)));
    assert.ok(Object.keys(deployed).length > 0);
    assert.deepEqual(options(oci ?? ''), deployed);
  });

  test('it leaves out provenance, as the deploy does', () => {
    assert.equal(actionBuild?.with?.provenance, false);
    assert.match(command, /--provenance=false/);
  });

  test('the comparison sees a difference in any option', () => {
    const deployed = options('type=image,push=true,oci-mediatypes=true,compression=gzip,force-compression=true');
    assert.deepEqual(options('type=oci,dest=x.tar,oci-mediatypes=true,compression=gzip,force-compression=true'), deployed);
    assert.notDeepEqual(options('type=oci,dest=x.tar,oci-mediatypes=true,compression=zstd,force-compression=true'), deployed);
    assert.notDeepEqual(options('type=oci,dest=x.tar,oci-mediatypes=true,compression=gzip'), deployed);
    // A value may itself hold '='; it is cut at the first one only.
    assert.deepEqual(options('annotation.a=b=c'), { 'annotation.a': 'b=c' });
  });

  // buildx could take an option and not act on it. The tarball's manifest
  // shows what the deploy's output would push: OCI media types and gzip
  // layers, the base's included, which is what soci indexes.
  test('a later step checks the tarball\'s manifest is OCI and every layer is OCI gzip', () => {
    const at = steps.indexOf(build!);
    const check = steps.slice(at + 1).find((s) => (s.run ?? '').includes('$RUNNER_TEMP/runner-image.tar'));
    assert.ok(check, 'a step after the build reads the tarball');
    assert.match(check!.run!, /application\/vnd\.oci\.image\.manifest\.v1\+json/);
    assert.match(check!.run!, /all\(\.layers\[\]; \.mediaType == "application\/vnd\.oci\.image\.layer\.v1\.tar\+gzip"\)/);
    assert.match(check!.run!, /jq -e /);
  });
});

describe('fitness: this repo\'s workflows run on GitHub\'s runners, never the ones it deploys (public-repo.md)', () => {
  test('every job names a GitHub-hosted Ubuntu runner, or calls another workflow', () => {
    assert.ok(workflows.length > 0);
    for (const { file, workflow: w } of workflows) {
      const jobs = w.jobs ?? {};
      for (const [name, job] of Object.entries(jobs) as [string, { 'runs-on'?: unknown; uses?: string; strategy?: { matrix?: { include?: Record<string, unknown>[] } } }][]) {
        if (job.uses) continue;
        // A matrix job's runner is each of its entries' runners, and every
        // entry must name one.
        const key = /^\$\{\{\s*matrix\.([\w-]+)\s*\}\}$/.exec(String(job['runs-on']))?.[1];
        const runners = key ? (job.strategy?.matrix?.include ?? []).map((entry) => entry[key]) : [job['runs-on']];
        assert.ok(runners.length > 0, `${file} ${name} names no runner`);
        for (const runner of runners) {
          assert.match(String(runner), /^ubuntu-(latest|\d{2}\.\d{2}(-arm)?)$/, `${file} ${name} runs on ${JSON.stringify(runner)}`);
        }
      }
    }
  });
});

// The deploy converts the runner's index with a SOCI index for each
// architecture and pushes it. Two deploys failed at that push after every
// offline test passed, so the gate runs the deploy's own scripts, with the
// real soci, skopeo and crane, on every pull request: against a registry
// started inside the job, never ECR, on an index of two images joined as
// the deploy joins them.
describe('the gate\'s soci job runs the deploy\'s convert and push for real', () => {
  const job = workflow.jobs?.soci;
  const steps = stepsOf(workflow, 'soci');
  const runs = steps.map((s) => s.run ?? '').join('\n');

  test('on a GitHub runner, with a registry of its own and no secret', () => {
    assert.equal(job?.name, 'soci');
    assert.equal(job?.['runs-on'], 'ubuntu-24.04');
    assert.match(String(job?.services?.registry?.image), /^registry:2@sha256:[0-9a-f]{64}$/);
    assert.deepEqual(job?.services?.registry?.ports, ['5000:5000']);
    assert.doesNotMatch(JSON.stringify(job), /secrets\.|aws-actions/);
  });

  test('installs the tools and converts, pushes and checks with the deploy\'s scripts', () => {
    assert.match(runs, /scripts\/soci\/install\.sh /);
    assert.match(runs, /scripts\/soci\/convert-and-push\.sh /);
    assert.match(runs, /jq -e -f scripts\/soci\/check-index\.jq/);
  });

  test('joins two single-platform images with imagetools, as the deploy joins the runner\'s', () => {
    assert.match(runs, /docker buildx imagetools create --tag /);
    assert.match(runs, /linux\/amd64/);
    assert.match(runs, /linux\/arm64/);
    assert.match(runs, /compression=gzip,force-compression=true/);
  });
});
