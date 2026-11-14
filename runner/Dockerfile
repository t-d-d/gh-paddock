# The runner's image: every pool's runner tasks run it, on x64
# (docs/constraints/runner-image.md). It starts FROM GitHub's official runner
# image and adds the CLI tools common workflows call without a setup action:
# the AWS CLI, jq, zip, Python 3, and the system libraries Chromium needs.
#
# Built on an x64 runner by the deploy, and by the gate, which starts its
# runner. Dependabot moves the tag below; GitHub stops sending jobs to a
# runner more than 30 days behind its latest release once updates are off,
# and the scale set turns them off.
#
# It copies nothing from the repo, so the deploy's tag hashes runner/ alone.
# The listener's task definition for each runner injects its config as
# ACTIONS_RUNNER_INPUT_JITCONFIG, which run.sh reads, runs one job, and
# exits.
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
    curl -fsSL https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o /tmp/awscli.zip; \
    unzip -q /tmp/awscli.zip -d /tmp; \
    /tmp/aws/install; \
    rm -rf /tmp/aws /tmp/awscli.zip; \
    aws --version

USER runner
CMD ["/home/runner/run.sh"]
