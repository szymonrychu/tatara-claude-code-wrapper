#!/usr/bin/env bash
# Guard: every hardcoded name this repo puts in agent-facing prompt text still
# exists upstream, checked BEFORE merge, offline of harbor and buildkit.
#
# The Dockerfile `test-guard` stage already checks the stronger version of this
# against the live MCP server. It cannot run on a pull_request: it is outside
# the runtime DAG (nothing COPY --from's it), the only thing that reaches it is
# build.sh's explicit --opt target=test-guard, ci.yml sets enable-image: false,
# and image-verify passes no --opt target= so it would solve the default target
# anyway. The Go tests behind it skip without a baked cli, so on every PR
# TestAgentPromptToolNamesAreAdvertised is green by skip. #193.
#
# This is the compensating control, in the pattern check-go-version-pins.sh
# established for the Go-pin half of the same stage. Two public tokenless HTTPS
# GETs, no harbor credential, nothing buildkit touches - i.e. none of the things
# the 401 on harbor.szymonrichert.pl/containers/tatara-cli proved unavailable to
# a PR-triggered job.
#
# IT IS STRICTLY WEAKER THAN THE STAGE AND DOES NOT REPLACE IT. tool-manifest.json
# carries tool names and enums and NO profile data: a tool granted to no profile
# passes here and fails the live oracle. Do not delete the stage on the strength
# of this script.
#
# Fail-closed everywhere: a fetch that 404s, a manifest with no tools, an absent
# agents dir and an empty prompt scan are all exit 1. A guard that warns and
# returns 0 on an unusable oracle reports "no name is stale" for the same reason
# a broken grep does.
set -euo pipefail

root="${1:-.}"
cd "$root"

CLI_REPO="szymonrychu/tatara-cli"
SKILLS_REPO="szymonrychu/tatara-agent-skills"

# --- 1. the pin this repo declares is ONE value -------------------------------
#
# build.sh forwards its own copy as --opt build-arg:TATARA_CLI_VERSION, so ITS
# literal is the one the released image actually gets. Checking the roster
# against a different copy than the one that ships is the TATARA_SKILLS_REF /
# tatara-helmfile#397 failure one variable over - and unlike that variable, this
# one has no single-source guard, because all three copies are legitimate here
# (the Makefile and build.sh defaults drive local and CI builds respectively).
# What must hold is that they AGREE.
#
# A "definition" is a line binding the variable to a vX.Y.Z literal. Each file
# must contribute exactly one, so a comment that mentions both the variable name
# and a version literal reds this - reword the comment.
SOURCES=(Dockerfile Makefile .github/ci/build.sh)
for f in "${SOURCES[@]}"; do
  if [ ! -f "$f" ]; then
    echo "expected build-pin source '${f}' does not exist - this guard's coverage is a hardcoded file list, so a rename must update SOURCES here rather than silently dropping the file"
    exit 1
  fi
done

pins=""
for f in "${SOURCES[@]}"; do
  hits="$(grep -E 'TATARA_CLI_VERSION' "$f" | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' || true)"
  n="$(printf '%s' "$hits" | grep -c . || true)"
  if [ "$n" -ne 1 ]; then
    echo "TATARA_CLI_VERSION must be bound to exactly one vX.Y.Z literal in ${f}; found ${n}:"
    printf '%s\n' "$hits"
    exit 1
  fi
  pins="${pins}${hits}"$'\n'
done

distinct="$(printf '%s' "$pins" | grep -c . || true)"
unique="$(printf '%s' "$pins" | sort -u | grep -c . || true)"
if [ "$unique" -ne 1 ]; then
  echo "the ${distinct} TATARA_CLI_VERSION pins disagree; build.sh's copy is the one forwarded as --build-arg, so the image would ship a cli this guard never checked:"
  paste -d' ' <(printf '%s\n' "${SOURCES[@]}") <(printf '%s' "$pins")
  exit 1
fi
CLI_VERSION="$(printf '%s' "$pins" | sort -u | head -n1)"

# The skills ref has exactly one build-time source by construction
# (check-skills-pin-single-source.sh, tatara-helmfile#397): the Dockerfile ARG.
SKILLS_REF="$(grep -E '^ARG TATARA_SKILLS_REF=' Dockerfile | head -n1 | cut -d= -f2)"
if [ -z "$SKILLS_REF" ]; then
  echo "could not read the TATARA_SKILLS_REF ARG default out of the Dockerfile"
  exit 1
fi

echo "pinned tatara-cli=${CLI_VERSION} tatara-agent-skills=${SKILLS_REF}"

# NOTE the bound the skills half buys. This ARG is only the runtime ENV DEFAULT:
# tatara-operator's internal/agent/pod.go sets TATARA_SKILLS_REF from
# Project.spec.agent.skillsRef unconditionally, so the Project pin wins in a real
# pod. This guards the ref THIS repo declares, not the one the fleet runs.

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# --- 2. tatara-cli's published tool manifest ----------------------------------
#
# Pinned to CLI_VERSION, never `latest`: `latest` would red this repo on a
# wrapper PR that changed nothing, every time tatara-cli cuts a release. The
# pinned tag is the cli the image bakes, and it is the one the guard is about.
# Public and tokenless (tatara-cli release.yml uploads it as a release asset;
# tatara-agent-skills' validate_tool_calls.py consumes the same URL by tag).
MANIFEST_URL="https://github.com/${CLI_REPO}/releases/download/${CLI_VERSION}/tool-manifest.json"
echo "fetching ${MANIFEST_URL}"
if ! curl -sSfL --retry 3 --retry-delay 2 -o "${work}/tool-manifest.json" "$MANIFEST_URL"; then
  echo "could not fetch tatara-cli's tool manifest at ${CLI_VERSION}. This is exit 1, not a warning: an unreachable oracle advertises no tools, and a guard that continues past it passes every name."
  exit 1
fi

# --- 3. the skills plugin's typed-subagent listing ----------------------------
#
# codeload, not api.github.com/repos/.../contents: the API is 60 requests/hour
# per IP unauthenticated and the ARC runners share one egress IP, so this would
# fail on a busy afternoon for reasons that have nothing to do with the tree.
SKILLS_URL="https://codeload.github.com/${SKILLS_REPO}/tar.gz/refs/tags/${SKILLS_REF}"
echo "fetching ${SKILLS_URL}"
if ! curl -sSfL --retry 3 --retry-delay 2 -o "${work}/skills.tar.gz" "$SKILLS_URL"; then
  echo "could not fetch tatara-agent-skills at ${SKILLS_REF}. This is exit 1, not a warning: see above."
  exit 1
fi

# --strip-components=3 drops the tarball's <repo>-<version>/.claude/agents/
# prefix, so the *.md land flat in ${work}/agents - the real clone shape
# installAgents reads, not a directory a test invented.
mkdir -p "${work}/agents"
if ! tar -xzf "${work}/skills.tar.gz" -C "${work}/agents" --strip-components=3 --wildcards '*/.claude/agents/*.md'; then
  echo "the tatara-agent-skills tarball at ${SKILLS_REF} has no .claude/agents/ - either the plugin moved the typed subagents or the ref is wrong. Exit 1 rather than reconcile against an empty roster."
  exit 1
fi

# --- 4. reconcile -------------------------------------------------------------
#
# A real main, not an env-gated test: there are no skip semantics anywhere in
# this path, so it cannot go green by running nothing the way the live guards do
# on a laptop.
mise exec -- go run ./cmd/namesguard \
  -root=. \
  -tool-manifest="${work}/tool-manifest.json" \
  -agents-dir="${work}/agents"
