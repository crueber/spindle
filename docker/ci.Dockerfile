# syntax=docker/dockerfile:1
# The CI toolchain image: everything the merge gate needs, installed once.
#
# Why: the gate used to run `apt-get update && apt-get install git nodejs npm
# build-essential chromium …` on every push and pull request, hammering the Debian
# mirror and paying the download on each run. This image is built when the pins
# below move and is referenced by digest from both CI systems.
#
# Rebuild with scripts/publish/ci-base.yml (GitHub) or:
#   docker build -f docker/ci.Dockerfile -t ghcr.io/crueber/spindle-ci:sid .
#   docker push ghcr.io/crueber/spindle-ci:sid
# then pin the resulting digest in .forgejo/workflows/ci.yml and scripts/publish/ci.yml.
FROM debian:sid@sha256:e276677157e13bc51ea5b8651f54ad28883f503cc37b3e3af2c4b6cc2f4efe7b

LABEL org.opencontainers.image.title="spindle CI toolchain"
LABEL org.opencontainers.image.description="Debian sid with the spindle merge-gate toolchain preinstalled (git 2.5x, node, gcc, chromium)"
LABEL org.opencontainers.image.source="https://github.com/crueber/spindle"

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      git \
      ca-certificates \
      curl \
      nodejs \
      npm \
      build-essential \
      openssh-client \
      chromium \
      fonts-dejavu-core \
 && rm -rf /var/lib/apt/lists/*

# Fail the build, not the pipeline, if a tool the gate asserts is missing.
RUN git --version && node --version && npm --version && gcc --version && chromium --version

WORKDIR /src
