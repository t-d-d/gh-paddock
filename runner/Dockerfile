# The runner's image: every pool's runner tasks run it, on x64 or arm64 as
# the pool sets (docs/constraints/runner-image.md). It starts FROM GitHub's
# official runner image and adds the CLI tools common workflows call without
# a setup action: the AWS CLI, jq, zip, Python 3, and the system libraries
# Chromium needs.
#
# Built once for each architecture, each on a GitHub runner of that
# architecture and never under emulation, by the deploy and by the gate,
# which starts its runner. GitHub publishes the base for both, so this file
# names no platform: the builder's own architecture picks the base's layer.
# Dependabot moves the tag below; GitHub stops sending jobs to a runner more
# than 30 days behind its latest release once updates are off, and the scale
# set turns them off.
#
# The deploy pushes it with every layer recompressed as zstd, which Fargate
# pulls and unpacks faster than gzip, so a cold runner task starts sooner.
#
# It copies nothing from the repo, so the deploy's tag hashes runner/ alone:
# the entrypoint below is written inline. The listener's task definition for
# each task injects its first config as PADDOCK_CONFIG, and the entrypoint
# runs a runner for it and for each later config the task is handed.
FROM ghcr.io/actions/actions-runner:2.338.2

USER root

# Ubuntu 24.04 renamed some of Chromium's libraries with a t64 suffix; take
# whichever name this base offers.
RUN set -eux; \
    apt-get update; \
    pkgs="jq zip unzip curl ca-certificates python3 python3-pip python3-venv fonts-liberation \
      libnss3 libnspr4 libdrm2 libxkbcommon0 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 \
      libgbm1 libpango-1.0-0 libcairo2 libx11-xcb1 libxshmfence1 libxss1 libgtk-3-0"; \
    for lib in libasound2 libatk1.0-0 libatk-bridge2.0-0 libcups2; do \
      if apt-cache show "${lib}t64" >/dev/null 2>&1; then pkgs="$pkgs ${lib}t64"; else pkgs="$pkgs $lib"; fi; \
    done; \
    apt-get install -y --no-install-recommends $pkgs; \
    rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    case "$(uname -m)" in \
      x86_64) cli=awscli-exe-linux-x86_64.zip ;; \
      aarch64) cli=awscli-exe-linux-aarch64.zip ;; \
      *) echo "no AWS CLI for $(uname -m)"; exit 1 ;; \
    esac; \
    curl -fsSL "https://awscli.amazonaws.com/${cli}" -o /tmp/awscli.zip; \
    unzip -q /tmp/awscli.zip -d /tmp; \
    /tmp/aws/install; \
    rm -rf /tmp/aws /tmp/awscli.zip; \
    aws --version

# Runs a runner for each config the task is handed
# (docs/constraints/runner-image.md). PADDOCK_CONFIG, injected by ECS from
# the task's own parameter, holds the first runner's name and just-in-time
# config, and a presigned URL to the one object that holds the task's later
# configs. After each job it waits up to 30 seconds for a new config, wipes
# the last job's checkout and temporary files, keeping the caches, and runs
# the next. It exits when no new config comes, or the URL is refused.
RUN cat > /home/runner/paddock-entrypoint <<'PADDOCK_ENTRYPOINT'
#!/bin/bash
set -euo pipefail

wait_for=${PADDOCK_CONFIG_WAIT:-30}
first=${PADDOCK_CONFIG:?the task was given no config}
# The URL reads the task's later configs: keep it out of the runners'
# environment. A job can still read this process's first environment, but
# only a job of the same pool, which is the boundary (pool-per-repo.md).
unset PADDOCK_CONFIG PADDOCK_CONFIG_WAIT
url=$(jq -er .url <<<"$first")
runner=$(jq -er .runner <<<"$first")
jit=$(jq -er .jitconfig <<<"$first")
unset first
ran=" $runner "

# The checkout, the job's temporary files, downloaded actions and the last
# runner's own registration go. The tool cache and the home caches stay
# (docs/constraints/pool-per-repo.md). A job can leave files only root may
# delete, so sudo, which the base image gives the runner user, deletes what
# rm cannot. If that fails too, the task exits before any runner starts.
wipe() {
  local paths=("$HOME/.runner" "$HOME/.credentials" "$HOME/.credentials_rsaparams")
  if [ -d "$HOME/_work" ]; then
    mapfile -t -O "${#paths[@]}" paths < <(find "$HOME/_work" -mindepth 1 -maxdepth 1 ! -name _tool)
  fi
  mapfile -t -O "${#paths[@]}" paths < <(find "${TMPDIR:-/tmp}" -mindepth 1 -maxdepth 1)
  rm -rf -- "${paths[@]}" 2>/dev/null || sudo -n rm -rf -- "${paths[@]}"
}

# Polls the URL for a config with a runner not yet run. A missing object is
# no config yet, and so is a server error or a network failure; any other
# refusal ends the wait.
next_config() {
  local deadline=$((SECONDS + wait_for)) body code name next
  body=$(mktemp)
  while ((SECONDS < deadline)); do
    code=$(curl -sS -o "$body" -w '%{http_code}' --max-time 5 "$url" 2>/dev/null) || code=000
    case $code in
      200)
        name=$(jq -r '.runner // empty' "$body" 2>/dev/null) || name=""
        # This runs under ||, where set -e does not hold, so a config with
        # no just-in-time config is checked here and counts as none.
        if [ -n "$name" ] && [[ $ran != *" $name "* ]] &&
          next=$(jq -er '.jitconfig | select(type == "string" and length > 0)' "$body" 2>/dev/null); then
          jit=$next
          runner=$name
          ran="$ran$name "
          rm -f "$body"
          return 0
        fi
        ;;
      000 | 404 | 5??) ;;
      *)
        echo "paddock: the config URL was refused ($code)"
        rm -f "$body"
        return 1
        ;;
    esac
    sleep 1
  done
  rm -f "$body"
  echo "paddock: no new config in ${wait_for}s"
  return 1
}

# ECS stops a task with SIGTERM to this script alone, which as process 1
# ignores any signal it has no handler for. A waiting task, with no runner
# registered, exits at once. A runner with no job yet is told to stop, and
# leaves GitHub. A runner whose job is running never sees the signal:
# Fargate Spot gives two minutes' notice, so a job near its end finishes,
# and the task then exits with no wait for another config. A job still
# running when the notice ends dies with the task, and the listener re-runs
# it once its workflow run completes.
#
# The runner runs each job in a process named Runner.Worker. job_running
# looks for one among run.sh's descendants alone, so no other process on
# the machine counts. The brackets keep grep from matching its own line.
job_running() {
  local -A parent=()
  local d s p workers=()
  for d in /proc/[0-9]*; do
    s=$(cat "$d/stat" 2>/dev/null) || continue
    s=${s##*) }
    read -r _ p _ <<<"$s"
    parent[${d#/proc/}]=$p
    if grep -qs 'Runner[.]Worker' "$d/cmdline"; then workers+=("${d#/proc/}"); fi
  done
  for p in "${workers[@]}"; do
    while [ -n "$p" ] && [ "$p" != 0 ]; do
      [ "$p" = "$child" ] && return 0
      [ "$p" = 1 ] && break
      p=${parent[$p]:-}
    done
  done
  return 1
}

child=""
stopping=""
stop() {
  trap - TERM INT
  stopping=1
  if [ -z "$child" ]; then
    exit 143
  fi
  if job_running; then
    echo "paddock: stop held back from runner $runner while its job runs"
  else
    kill -TERM "$child" 2>/dev/null || true
  fi
}
trap stop TERM INT

while :; do
  wipe
  echo "paddock: starting runner $runner"
  # run.sh registers the runner, runs one job, and leaves GitHub before it
  # returns, so the task never waits or exits with a runner registered.
  ACTIONS_RUNNER_INPUT_JITCONFIG=$jit "$HOME/run.sh" &
  child=$!
  code=0
  wait "$child" || code=$?
  # A trapped signal cuts the wait short: wait again for the job itself.
  while [ -n "$stopping" ] && kill -0 "$child" 2>/dev/null; do
    code=0
    wait "$child" || code=$?
  done
  child=""
  [ -z "$stopping" ] || exit 143
  ((code == 0)) || exit "$code"
  next_config || exit 0
done
PADDOCK_ENTRYPOINT
RUN chmod 755 /home/runner/paddock-entrypoint

USER runner
CMD ["/home/runner/paddock-entrypoint"]
