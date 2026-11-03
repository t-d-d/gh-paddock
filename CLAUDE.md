# Working in this repo

gh-paddock runs self-hosted GitHub Actions runners for private repos on AWS Fargate. One listener task polls GitHub's runner scale-set API and starts single-use runner tasks to match. It deploys to its own AWS account, the CI account.

- The listener is Go (`cmd/`, `internal/`). The repo's checks, the deploy's scripts and the tests of the stacks and workflows are Node (`scripts/`, `test/`).
- PRDs are issues labelled `type: prd`. There is no PRD file. A PRD's "Non-goals" list is binding: do not build one.
- [docs/constraints/](docs/constraints/README.md) holds the rules in force. Decisions and their reasons live in issues.

## Scope

- Work you find in passing becomes an issue. Do not fix it in the branch you are on.

## Before any code edit

- Run the `constraints` skill. The stacks, the workflows, the Dockerfile, `go.mod` and `package.json` count as code.
- If the task conflicts with a constraint, stop and say so. Do not work around the rule, and do not quietly follow the task.
- A change to what a constraint means needs an issue the owner agrees to, linked from the file's History line.

## Tests first

- Write the failing test first. Watch it fail for the reason you expect, then write the code.
- Go tests sit beside their package (`*_test.go`), run by `go test`. Node tests are `test/**/*.test.ts`, run by `node --test`.
- No test reaches the network, GitHub or AWS. Fake the client at its boundary.
- A fitness function's describe name starts `fitness:` and names its constraint file.
- Never weaken or delete a test to get the gate green.

## The gate

- `npm run gate` runs everything: the public-repo scan, lint, types, the Node tests, then `gofmt`, `go vet` and the Go tests. CI runs the same command on every push and pull request, and builds the listener's image.
- The gate is offline and holds no secrets.

## Branches and pull requests

- Never push to main. One branch per issue.
- Run the `pre-push` skill before every push. It ends by raising the pull request.
- The pull request body follows the `pr-description` skill.
- Nothing deploys except a green merge to main. Never deploy by hand.
- The owner merges. Do not merge your own pull request.

## Issues

- Given an issue number, run the `fetch-ticket` skill first.
- An issue is picked up as soon as it is labelled `mode: afk`, and goes from its text to a pull request without stopping to ask.
- Anything only the owner can do is not yours: a secret, a repository variable, the App's settings, an AWS setting, the list of served repos. Say what is needed and stop.
- When your pull request has merged or closed, or you are done and have none, make the last line of your reply `Signing off.` or `Signing off: <reason>`. That archives the session. Do not sign off while the owner is working with you.

## GitHub from a cloud session

- Read the `github-tooling` skill before any `gh` command.
- A session has no cloud credentials and cannot read the AWS account. If a task needs a value, stop and say so.

## The repo is public

- No file holds an account, installation, environment or session id, or names a served private repo ([public-repo.md](docs/constraints/public-repo.md)). Tests use fakes.
- No issue reference has four or more digits. A comment that needs a reason writes the reason, not an issue number.
- This repo's own workflows run on GitHub's runners, never on the runners it deploys.

## Writing

- Plain words and whole sentences. No em dashes.
- Run the `unslop` skill over anything a person will read: pull request bodies, issues, docs and the README.
