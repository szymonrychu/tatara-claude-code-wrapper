#!/usr/bin/env bash
# Build this repo's image via the shared rootless buildkitd daemon and push to
# harbor. Runs on the ARC runner (in-cluster, namespace arc-runners). Talks gRPC
# to the buildkitd Service; buildkitd writes all layers/cache to its Ceph PVC
# (--root), OFF the control-plane etcd NVMe. No in-cluster Job, no transient
# cluster secrets: harbor push auth is a per-build docker config on THIS runner,
# the private-repo clone token is a buildkit frontend secret. Replaces
# kaniko-build.sh.
set -euo pipefail

REPO="${1:?repo name required}"
BUILDKITD_ADDR="tcp://buildkitd.arc-runners:1234"
# BUILD_REF is the commit the image is built FROM (local git-describe VERSION,
# the remote buildkit context, and the image :SHORT_SHA tag all key off it).
# Defaults to GITHUB_SHA, which is correct on the ci push path where they
# coincide. The release (workflow_run) path MUST override it with the triggering
# run's head_sha: on workflow_run GITHUB_SHA points at the default-branch tip,
# which can have advanced past the commit being released, so pinning here keeps
# VERSION, the SHORT_SHA tag, and the cloned context all on the same commit.
BUILD_REF="${BUILD_REF:-$GITHUB_SHA}"
SHORT_SHA="${BUILD_REF:0:7}"
VERSION="$(git describe --tags --always --dirty)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# TATARA_CLI_VERSION pins the cli SHA baked into the image; keep in sync with
# Dockerfile ARG default and Makefile default.  Use the short SHA published by
# tatara-cli CI (both SHORT_SHA and VERSION tags are pushed on every main merge).
TATARA_CLI_VERSION="${TATARA_CLI_VERSION:-v0.4.1}"
# TATARA_SKILLS_REF pins the skills plugin ref baked as the runtime ENV default;
# keep in sync with the Dockerfile ARG default and Makefile default. Rewritten by
# the skills->wrapper cd-release bump.
TATARA_SKILLS_REF="${TATARA_SKILLS_REF:-v0.1.0}"
DEST="harbor.szymonrichert.pl/containers/${REPO}"

: "${GITHUB_TOKEN:?GITHUB_TOKEN required}"
: "${HARBOR_USERNAME:?HARBOR_USERNAME required}"
: "${HARBOR_PASSWORD:?HARBOR_PASSWORD required}"

# Per-build docker config on the runner only (never an in-cluster secret).
# buildctl reads $DOCKER_CONFIG and forwards harbor auth to buildkitd for push.
DOCKER_CONFIG="$(mktemp -d)"
export DOCKER_CONFIG
trap 'rm -rf "$DOCKER_CONFIG"' EXIT
auth="$(printf '%s:%s' "$HARBOR_USERNAME" "$HARBOR_PASSWORD" | base64 -w0)"
cat >"${DOCKER_CONFIG}/config.json" <<EOF
{"auths":{"harbor.szymonrichert.pl":{"auth":"${auth}"}}}
EOF

CONTEXT="https://github.com/szymonrychu/${REPO}.git#${BUILD_REF}"

# Build-guard FIRST (build-only, never pushed). The Dockerfile `test-guard`
# stage runs the MCP-tools flowthrough test with the pinned tatara cli on PATH;
# if the baked cli dropped a tool the wrapper relies on, the test fails and this
# command exits non-zero BEFORE the runtime image below is built/pushed. This is
# the explicit driver the guard needs: buildkit does dead-stage elimination, so
# building only the runtime target would never reach test-guard (it is not in
# the runtime DAG). `--output type=cacheonly` builds the stage without exporting
# an image; the work is cached for the runtime build that follows.
echo "buildkit: running cli MCP-tools build-guard (target=test-guard)"
buildctl --addr "$BUILDKITD_ADDR" build \
  --frontend dockerfile.v0 \
  --opt context="${CONTEXT}" \
  --opt filename=Dockerfile \
  --opt target=test-guard \
  --opt build-arg:TATARA_CLI_VERSION="${TATARA_CLI_VERSION}" \
  --secret id=GIT_AUTH_TOKEN,env=GITHUB_TOKEN \
  --output type=cacheonly

# Remote git context (buildkitd clones the private repo, like kaniko did).
# MUST be https:// (NOT git://): buildkit's GIT_AUTH_TOKEN basic-auth extraheader
# only engages over https, and github.com no longer serves the git:// protocol.
# GIT_AUTH_TOKEN is the buildkit git-source frontend secret for the private
# clone; it is NOT a build-arg, so it never lands in a layer.
buildctl --addr "$BUILDKITD_ADDR" build \
  --frontend dockerfile.v0 \
  --opt context="${CONTEXT}" \
  --opt filename=Dockerfile \
  --opt build-arg:VERSION="${VERSION}" \
  --opt build-arg:COMMIT="${SHORT_SHA}" \
  --opt build-arg:DATE="${BUILD_DATE}" \
  --opt build-arg:TATARA_CLI_VERSION="${TATARA_CLI_VERSION}" \
  --opt build-arg:TATARA_SKILLS_REF="${TATARA_SKILLS_REF}" \
  --secret id=GIT_AUTH_TOKEN,env=GITHUB_TOKEN \
  --output "type=image,\"name=${DEST}:${SHORT_SHA},${DEST}:${VERSION}\",push=true"

echo "buildkit: pushed ${DEST}:${SHORT_SHA} and ${DEST}:${VERSION}"
