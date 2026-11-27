# syntax=docker/dockerfile:1
# The image-publish job's toolchain: buildah (pinned by digest) with git installed
# at build time. The job used to run `apk add`/`dnf install git` on every push to
# main; that is a distribution-mirror download per run, so it lives here instead.
#
# Rebuild when the quay digest moves:
#   docker build -f docker/ci-buildah.Dockerfile -t github.com/crueber/spindle-ci-buildah:<date> .
#   docker push ghcr.io/crueber/spindle-ci-buildah:<date>
#   docker inspect --format='{{index .RepoDigests 0}}' github.com/crueber/spindle-ci-buildah:<date>
# and pin the digest in .forgejo/workflows/ci.yml (the `image` job).
FROM quay.io/buildah/stable@sha256:04901e724c5d1db62672c48472fe4b2b85daeb910d8a6f48fb5c4910dbba52bc

RUN if command -v apk >/dev/null 2>&1; then apk add --no-cache git; \
    elif command -v microdnf >/dev/null 2>&1; then microdnf install -y git && microdnf clean all; \
    elif command -v dnf >/dev/null 2>&1; then dnf install -y git && dnf clean all; \
    fi \
 && git --version

WORKDIR /src
