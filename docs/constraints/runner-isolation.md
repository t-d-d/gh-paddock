# Runner isolation
Relevance: Who holds the App key, who starts tasks, and what a runner task can read: cmd/listener, internal/github, internal/runners, the runner image's entrypoint in runner/, the roles and the runner-config bucket in infra/**
Decision: Only the listener holds the App key and starts or stops tasks; a runner task holds no role and reads only its own configs.

Why: A runner runs whatever a served repo's workflow says, so it is the part
most likely to run hostile code. If it could read the App key, it could
register runners for any repo on the org; if it could start tasks, it could
run up the bill or reach another repo's pool.

## Rules

- The App key lives in the SecureString parameter `/paddock/github-app`
  (public-repo.md). Only the listener's task role may read it, and that role
  reads no other parameter or secret of the operator's besides the repo list,
  `/paddock/repos`.
- Only the listener's task role may start tasks, and only from the
  `paddock-runner` task definition family, in the stack's own cluster. It may
  pass the runner's execution role and no other.
- Each runner's just-in-time config goes in its own SecureString parameter
  under `/paddock/runners/`. The listener registers a task definition for that
  one runner, which names that one parameter as a secret, and deletes the
  definition once the task is asked for. ECS injects the config as the task
  starts.
- A running task gets its next config through a presigned URL. The URL
  travels in the task's first config's SecureString parameter, beside the
  config, and nowhere else. It reaches one object in the stack's
  runner-config bucket, and the listener writes that task's later configs
  to that object alone. The URL is the task's only way to it: the task
  still holds no role and no AWS credentials.
- The warm limit is 30 minutes from the task's start. The listener hands a
  task no config after it. It signs the URL once, at the task's start, for
  a little over the limit, and never re-signs it. The URL lasts no longer
  than the listener's own credentials, so the task exits when the URL is
  refused, as it does when no config comes.
- A task never runs a config twice: it remembers each config's runner name
  and treats a repeat as no config.
- The bucket blocks public access and expires objects after a day.
- A runner task has no task role. Its execution role may read
  `/paddock/runners/*` for ECS, but the runner's own process holds no AWS
  credentials.
- A runner config is deleted once its task runs or is gone. The listener
  deletes a later config's object once that config's runner registers in
  GitHub, or once its task is gone.
- The App holds two permissions: the org's self-hosted runners, read and
  write, and the repos' Actions, read and write. Actions write is only for
  re-running a job that a Spot reclaim killed.

## Examples

- `fitness: only the listener reads the App key and starts tasks, and a runner reads only its own config (runner-isolation.md)`
  in test/stack.test.ts.
- The Go subtest `fitness: a runner task reads only its own config and holds no role (runner-isolation.md)`
  in internal/runners, and `fitness: a waiting task's later configs come only through its own URL (runner-isolation.md)`
  beside it.
- `fitness: the entrypoint reads later configs only through its URL, and runs none twice (runner-isolation.md)`
  in test/runner-entrypoint.test.ts.

## Do not

- Do not pass the config in the task's environment overrides: anyone who can
  describe the task can read them.
- Do not give runner tasks a task role to read their config: a role cannot be
  scoped to one task's parameter, so every runner could read every other's.
- Do not let the listener's role read `/paddock/*`: a later parameter of the
  operator's would come with it.
- Do not give a running task a credential to fetch its next config, or a URL
  to a prefix or a bucket: one task could then read another's configs.

Judgment: no check sees the App's permissions, which live in GitHub. The owner sets them; a change to them changes this file.
History: Settled with the owner in PRD #1, and written by #16. The App key's format is in #16's comments. #53 added the presigned URL that hands a running task its next config. #54 put the URL in the first config's parameter, set the warm limit, and had the task run no config twice.
