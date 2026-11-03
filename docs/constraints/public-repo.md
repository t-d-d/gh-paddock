# A public repo
Relevance: Any file that could hold a value: code, tests, fixtures, docs, the stacks, workflows, scripts/**
Decision: The repo is public, so no file holds an account or installation id, names a served private repo, or cites another repo's issue.

Why: Anyone can read and fork the repo, and read its Actions run logs. An
account or installation id is a foothold, and the list of private repos
these runners serve is the owner's business, not the repo's.

## Rules

- No file holds an AWS account id, a GitHub App installation id, or a
  claude.ai environment or session id. Tests use AWS's documented example
  account, 123456789012, and short fakes.
- Which repos the runners serve, their pools and their scale-set names are
  deploy-time configuration in the CI account. No file names a served
  private repo. A doc, meaning the README or Markdown under docs/, may name
  the service it builds, bare, and link its site.
- No issue or pull request reference has four or more digits: it would
  point at another repo's numbers.
- The stacks take the operator's values as parameters. The alert address
  is read from Parameter Store, `/paddock/alert-email`, so it never passes
  through GitHub: a step prints each value its env block is given, and
  Actions masks only a secret's exact string.
- The deploy's run log does not show the AWS account id. The deploy reads
  the deploy role's ARN, which carries the id, from the repository secret
  `AWS_DEPLOY_ROLE_ARN`, and its credentials step sets
  `mask-aws-account-id: true`, so every later line masks it. No step before
  the credentials step runs a command.
- The mask reaches only the log, not a run artifact or a job summary, and
  anyone signed in can download a public repo's artifacts. The image build
  sets `DOCKER_BUILD_RECORD_UPLOAD` and `DOCKER_BUILD_SUMMARY` to false, and
  no step of the image action or the deploy writes a job summary or uploads
  an artifact.
- What the listener logs names tasks by their ids and images by their
  digests, never by an ARN or a registry: log lines get pasted into public
  issues.
- The GitHub App's key lives in AWS Secrets Manager, `paddock/github-app`,
  in the bootstrap stack. Nothing reads it from GitHub.
- This repo's own workflows run on GitHub's runners, never on the runners
  it deploys, and the runner groups it serves leave public repos off: a
  fork's pull request must not reach a runner in the CI account.
- scripts/public-repo-scan.mjs refuses the ids and names above, in every
  tracked file and path, with no file exempted. A file that has to spell a
  refused value to test the scan builds it from parts.

## Examples

- The scan: `npm run scan`, the first step of `npm run gate`, and
  `fitness: no tracked file holds an id or names the served service outside a doc (public-repo.md)`
  in test/public-repo-fitness.test.ts.
- The deploy's log: `fitness: the deploy's log never shows the AWS account id (public-repo.md)`.
- The listener's environment carries its image's digest, `PADDOCK_IMAGE_DIGEST`,
  not its URI (infra/paddock.cfn.yml).

## Do not

- Do not list served repos in a file or a stack default: the list is
  private, and a change to it would need a pull request in public.
- Do not put a value in a step's env block that is not a secret or already
  public: the step prints it.
- Do not run this repo's workflows on its own runners: a fork's pull
  request would run code in the CI account.

Judgment: that a log line or an error message names an ARN or a registry no check sees; the scan reads files, not what the code prints.
History: Set with the repo's scaffold, November 2026, under PRD #1. Adapted from gh-governor's.
