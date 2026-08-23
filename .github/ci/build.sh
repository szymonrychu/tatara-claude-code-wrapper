#!/usr/bin/env bash
# Build this repo's image via the shared rootless buildkitd daemon and push to
# harbor. Runs on the ARC runner (in-cluster, namespace arc-runners). Talks gRPC
# to the buildkitd Service; buildkitd writes all layers/cache to its Ceph PVC
# (--root), OFF the control-plane etcd NVMe. No in-cluster Job, no transient
# cluster secrets: harbor push auth is a per-build docker config on THIS runner,
# the private-repo clone token is a buildkit frontend secret. Replaces
# kaniko-build.sh.
#
# usage: build.sh <repo> [guard|image|all]
#
# The two solves are separable so release.yml can put the GUARD ON THE OTHER
# SIDE OF `cut tag` (#193). The tag is pushed to origin by cd-release before
# build.sh ever ran, so a bump that drops a tool the prompt text names used to
# fail AFTER the tag was public, leaving the dangling "tag with no image and no
# chart" ci.yml's pin-guard comment describes. Splitting the modes MOVES the
# guard solve; it does not add one, so solves per release are unchanged.
#
# Default `all` keeps `make`/manual/local invocations behaving exactly as before.
set -euo pipefail

REPO="${1:?repo name required}"
MODE="${2:-all}"
case "$MODE" in
  guard | image | all) ;;
  *)
    echo "usage: $0 <repo> [guard|image|all]"
    exit 2
    ;;
esac
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
# The release job passes the authoritative tag in via VERSION; git describe
# is only the ci-path fallback (no release tag cut yet). git describe must not
# be trusted to pick the release tag itself: a re-run of a release that failed
# after cutting its tag leaves two semver tags on the same commit, and describe
# resolves to the LOWER one, publishing the image under the wrong version.
#
# Deliberately NOT resolved in guard mode. That mode now runs BEFORE `cut tag`,
# where git describe would resolve to the PREVIOUS release - a value nothing
# reads, but one that would look authoritative in a log next to a version that
# does not exist yet.
if [ "$MODE" != "guard" ]; then
  VERSION="${VERSION:-$(git describe --tags --always --dirty)}"
fi
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# TATARA_CLI_VERSION pins the cli SHA baked into the image; keep in sync with
# Dockerfile ARG default and Makefile default.  Use the short SHA published by
# tatara-cli CI (both SHORT_SHA and VERSION tags are pushed on every main merge).
TATARA_CLI_VERSION="${TATARA_CLI_VERSION:-v4.0.0}"
# There is deliberately NO TATARA_SKILLS_REF default here, and the build below
# passes no build-arg for it (tatara-helmfile#397). The skills->wrapper
# cd-release bump rewrites the DOCKERFILE ARG only, so a default here could
# only drift - and it did: this file's literal v2.1.0 was forwarded
# unconditionally and OVERRODE the CD-written Dockerfile pin on every release
# build, making that whole CD edge inert. The Dockerfile ARG default is now the
# single build-time source.
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

# Build-guard FIRST (build-only, never pushed), and in `guard` mode BEFORE the
# tag is cut. The Dockerfile `test-guard`
# stage runs the MCP-tools flowthrough test with the pinned tatara cli on PATH,
# plus the prompt-text guard (#136) that checks every MCP tool name this repo
# puts in front of an agent against that same live tools/list; if the baked cli
# dropped a tool the wrapper relies on or names in the CLAUDE.md it injects, the
# test fails and this command exits non-zero BEFORE the runtime image below is
# built/pushed. This is
# the explicit driver the guard needs: buildkit does dead-stage elimination, so
# building only the runtime target would never reach test-guard (it is not in
# the runtime DAG). Omitting --output builds the target stage for its side
# effects (the `RUN go test` guard) and discards the result; buildkitd still
# caches the solved layers for the runtime build that follows. (There is no
# `cacheonly` exporter registered on the buildkitd daemon, so `--output
# type=cacheonly` fails with "exporter cacheonly could not be found".)
if [ "$MODE" != "image" ]; then
  echo "buildkit: running cli MCP-tools build-guard (target=test-guard)"
  buildctl --addr "$BUILDKITD_ADDR" build \
    --frontend dockerfile.v0 \
    --opt context="${CONTEXT}" \
    --opt filename=Dockerfile \
    --opt target=test-guard \
    --opt build-arg:TATARA_CLI_VERSION="${TATARA_CLI_VERSION}" \
    --secret id=GIT_AUTH_TOKEN,env=GITHUB_TOKEN
fi

if [ "$MODE" = "guard" ]; then
  echo "buildkit: guard-only mode; skipping the runtime image build"
  exit 0
fi

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
  --secret id=GIT_AUTH_TOKEN,env=GITHUB_TOKEN \
  --output "type=image,\"name=${DEST}:${SHORT_SHA},${DEST}:${VERSION}\",push=true"

echo "buildkit: pushed ${DEST}:${SHORT_SHA} and ${DEST}:${VERSION}"
