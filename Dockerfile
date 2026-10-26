# forksync runtime image. goreleaser puts the pre-built `forksync` binary in
# the build context (see `dockers` in .goreleaser.yaml); nothing is compiled
# here. Local build: `make image`.
#
# Usable as an Azure DevOps `container:` job image: glibc-based (the agent
# mounts its own Node.js), no ENTRYPOINT, runs as root so the agent can add
# its user. Plain `docker run` therefore needs the command spelled out:
#   docker run --rm -v "$PWD:/work" -w /work ghcr.io/helayoty/forksync:vX.Y.Z forksync lint

# TODO: pin digest (debian:bookworm-slim@sha256:...); no registry access when this was written.
FROM debian:bookworm-slim AS base
# gh from the GitHub CLI apt repo; safe.directory because mounted workspaces
# are usually owned by another uid.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl git tar bash; \
    install -d -m 0755 /etc/apt/keyrings; \
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /etc/apt/keyrings/githubcli-archive-keyring.gpg; \
    chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends gh; \
    rm -rf /var/lib/apt/lists/*; \
    git config --system --add safe.directory '*'

FROM base
ARG VERSION=dev
LABEL org.opencontainers.image.title="forksync" \
      org.opencontainers.image.description="Keeps a fork as upstream tag + a rebased patch stack" \
      org.opencontainers.image.source="https://github.com/helayoty/forksync" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY forksync /usr/local/bin/forksync
WORKDIR /work
CMD ["forksync"]
