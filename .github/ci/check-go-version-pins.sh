#!/usr/bin/env bash
# Guard: Dockerfile's GO_VERSION, go.mod's go directive, and .mise.toml's go
# pin do not disagree.
#
# No shared PR job reads the Dockerfile ARG: every job runs jdx/mise-action
# against .mise.toml only, and this repo's enable-image-verify is false
# (tatara-operator#640 - Dockerfile:57 pulls a private Harbor image the PR
# path cannot carry credentials for, so the final stage is never built on a
# PR). That means nothing on the PR path ever compiles the Dockerfile, and a
# GO_VERSION left behind a go.mod bump reproduces tatara-operator#556 exactly
# ("go.mod requires go >= X, running go Y") - but only once release.yml
# builds the image for real, which is too late. This is the local, offline
# stand-in for that one class of defect.
#
# What this does NOT cover: a renamed base image, a dropped apk package, a
# moved COPY source, the npm engine range, the mise python pin - anything a
# full Dockerfile compile would catch and a version-string check cannot. It
# exists only because enable-image-verify is off here; it is not a substitute
# for that job.
#
# Offline, no token, no network. Run from the repo root.
set -euo pipefail

root="${1:-.}"
cd "$root"

fail=0

DOCKERFILE=Dockerfile
GOMOD=go.mod
MISE=.mise.toml

for f in "$DOCKERFILE" "$GOMOD" "$MISE"; do
  if [ ! -f "$f" ]; then
    echo "expected source '${f}' does not exist - this guard's coverage is a hardcoded file list, so a rename must update it here rather than silently dropping the file"
    fail=1
  fi
done
[ "$fail" -eq 0 ] || exit 1

# Extract Dockerfile's ARG GO_VERSION default. If the ARG has moved or been
# renamed, that is a failure, not a silent pass: a grep that matches nothing
# is a check that stopped checking.
dockerfile_line="$(grep -n '^ARG GO_VERSION=' "$DOCKERFILE" || true)"
if [ -z "$dockerfile_line" ]; then
  echo "no 'ARG GO_VERSION=' line found in ${DOCKERFILE} - the pin has moved or been renamed and this guard cannot find it"
  fail=1
else
  dockerfile_ln="${dockerfile_line%%:*}"
  dockerfile_ver="$(printf '%s\n' "$dockerfile_line" | sed -E 's/^[0-9]+:ARG GO_VERSION=//')"
fi

# Extract go.mod's `go` directive.
gomod_line="$(grep -n '^go [0-9]' "$GOMOD" || true)"
if [ -z "$gomod_line" ]; then
  echo "no 'go X.Y.Z' directive found in ${GOMOD} - the directive has moved or changed shape and this guard cannot find it"
  fail=1
else
  gomod_ln="${gomod_line%%:*}"
  gomod_ver="$(printf '%s\n' "$gomod_line" | sed -E 's/^[0-9]+:go //')"
fi

# Extract .mise.toml's go pin (inside [tools]).
mise_line="$(grep -n '^go = "' "$MISE" || true)"
if [ -z "$mise_line" ]; then
  echo "no 'go = \"X.Y.Z\"' pin found in ${MISE} - the pin has moved or changed shape and this guard cannot find it"
  fail=1
else
  mise_ln="${mise_line%%:*}"
  mise_ver="$(printf '%s\n' "$mise_line" | sed -E 's/^[0-9]+:go = "([^"]+)"/\1/')"
fi

[ "$fail" -eq 0 ] || exit 1

# (a) Dockerfile's GO_VERSION must be >= go.mod's go directive. GOTOOLCHAIN=local
# in the Dockerfile means a builder older than go.mod fails `go mod download`
# outright (Dockerfile:3-5), so this direction must never regress. `>=`, not
# `==`: a go.mod directive of "1.26" (no patch) is legitimate and must not red
# against a Dockerfile pin of "1.26.3".
if ! printf '%s\n%s\n' "$gomod_ver" "$dockerfile_ver" | sort -V -C; then
  echo "${DOCKERFILE}:${dockerfile_ln} ARG GO_VERSION=${dockerfile_ver} is older than ${GOMOD}:${gomod_ln} 'go ${gomod_ver}' - a builder older than go.mod fails 'go mod download' (GOTOOLCHAIN=local), this is tatara-operator#556's failure class"
  fail=1
fi

# (b) Dockerfile's GO_VERSION must equal .mise.toml's go pin exactly. This is
# the invariant Dockerfile:3-5 already states in prose, and .mise.toml is what
# every PR-path job actually installs via jdx/mise-action - so this is the one
# comparison standing in for "the Dockerfile would build with what CI ran".
if [ "$dockerfile_ver" != "$mise_ver" ]; then
  echo "${DOCKERFILE}:${dockerfile_ln} ARG GO_VERSION=${dockerfile_ver} does not match ${MISE}:${mise_ln} go = \"${mise_ver}\" - these must be exactly equal, since .mise.toml is what the PR path actually runs"
  fail=1
fi

# (c) No FROM golang: line may carry a literal version - all must interpolate
# ${GO_VERSION}, or (a) and (b) above are trivially bypassable by a stage that
# just ignores the ARG.
# shellcheck disable=SC2016 # literal search for the string ${GO_VERSION}, not shell expansion
literal_from="$(grep -n '^FROM golang:' "$DOCKERFILE" | grep -v '\${GO_VERSION}' || true)"
if [ -n "$literal_from" ]; then
  echo "FROM golang: line(s) in ${DOCKERFILE} do not interpolate \${GO_VERSION} - a literal version here bypasses the ARG GO_VERSION checks above entirely:"
  printf '%s\n' "$literal_from"
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "go version pin guard: ok (Dockerfile ARG GO_VERSION=${dockerfile_ver}, go.mod go ${gomod_ver}, .mise.toml go = \"${mise_ver}\")"
