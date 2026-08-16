#!/usr/bin/env bash
# Guard: TATARA_SKILLS_REF has exactly ONE build-time source, the Dockerfile ARG.
#
# That ARG is the only one the skills->wrapper cd-release bump rewrites. Before
# tatara-helmfile#397 two more copies existed - a Makefile default and a
# build.sh default - and build.sh forwarded its copy as an unconditional
# --build-arg, so the hand-maintained v2.1.0 OVERRODE the CD-written Dockerfile
# pin on every release build. The CD edge wrote a value no image ever carried
# and no agent ever read. Nothing went red; there was nothing looking.
#
# Re-adding a default anywhere but the Dockerfile, or re-adding a --build-arg
# that forwards one, silently restores that. This is the thing looking.
#
# Offline, no token, no network. Run from the repo root.
set -euo pipefail

root="${1:-.}"
cd "$root"

fail=0

# A "definition" is a line that binds TATARA_SKILLS_REF to a literal ref: a
# vX.Y.Z tag or a PENDING- sentinel. `ENV TATARA_SKILLS_REF=${TATARA_SKILLS_REF}`
# and the bare `ARG TATARA_SKILLS_REF` re-declaration bind no literal and are
# not definitions.
definitions="$(grep -REn 'TATARA_SKILLS_REF' Dockerfile Makefile .github/ci/build.sh \
  | grep -E 'v[0-9]+\.[0-9]+\.[0-9]+|PENDING-' || true)"

count="$(printf '%s' "$definitions" | grep -c . || true)"

if [ "$count" -ne 1 ]; then
  echo "TATARA_SKILLS_REF must be defined in exactly one place (Dockerfile ARG); found ${count}:"
  printf '%s\n' "$definitions"
  fail=1
elif ! printf '%s' "$definitions" | grep -q '^Dockerfile:'; then
  echo "TATARA_SKILLS_REF's single definition is not in the Dockerfile - the skills cd-release bump rewrites the Dockerfile ARG and nothing else:"
  printf '%s\n' "$definitions"
  fail=1
fi

# A --build-arg for it overrides the Dockerfile ARG default even when the value
# is empty, so this must stay absent regardless of where the definition lives.
if forwards="$(grep -REn 'build-arg:?[= ]?TATARA_SKILLS_REF' Makefile .github/ci/build.sh || true)"; [ -n "$forwards" ]; then
  echo "a build-arg forwards TATARA_SKILLS_REF, overriding the CD-written Dockerfile ARG:"
  printf '%s\n' "$forwards"
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "TATARA_SKILLS_REF single-source guard: ok"
